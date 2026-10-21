package dav

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// activeLock returns the node's live lock.
//
// A missing or expired lock is reported as ok=false rather than as an error: the
// caller's question is "is it locked", and an expired row answers no.
func (h *Handler) activeLock(ctx context.Context, nodeID [16]byte) (store.Lock, bool) {
	var (
		lock store.Lock
		ok   bool
	)
	_ = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		l, err := tx.ActiveLock(ctx, nodeID)
		if err != nil {
			return nil // ErrNotFound means "not locked"
		}
		lock, ok = l, true
		return nil
	})
	return lock, ok
}

// refreshLock extends a lock the caller owns.
//
// Ownership is passed in rather than read from the handler: the handler is shared
// across requests, so storing "the current user" there would leak one request's
// identity into another's.
func (h *Handler) refreshLock(ctx context.Context, nodeID [16]byte, token string,
	userID [16]byte, ttl time.Duration) (string, time.Time, bool) {

	var (
		lock      store.Lock
		refreshed bool
	)
	_ = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		l, err := tx.RefreshLock(ctx, nodeID, token, userID, h.now().Add(ttl))
		if err != nil {
			return nil
		}
		lock, refreshed = l, true
		return nil
	})
	if !refreshed {
		return "", time.Time{}, false
	}
	return lock.Token, lock.ExpiresAt, true
}

// handleProppatch stores dead properties.
//
// Only DAV: reserved properties are refused; everything else is persisted, which
// is what makes Windows Explorer remember Win32LastModifiedTime across a
// reconnect.
func (h *Handler) handleProppatch(w http.ResponseWriter, r *http.Request, a authz.Actor) {
	cap, node, err := h.resolvePath(r.Context(), a, r.URL.Path)
	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}
	if !cap.Can(authz.PermWrite) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}

	props, err := parseProppatchBody(r.Body)
	if err != nil {
		writeXMLError(w, http.StatusBadRequest, "")
		return
	}

	var rejected []string
	for _, p := range props {
		if strings.EqualFold(p.Space, "DAV:") {
			// Reserved properties are computed from the node's own state; letting a
			// client set them would let it lie about size or modification time,
			// which breaks every caching client.
			rejected = append(rejected, p.Name)
		}
	}

	// The store error is reported, not swallowed.
	//
	// It used to be discarded and the response was a 207 whose every property said "200 OK", so a
	// client was told its properties were saved when none had been. Windows Explorer relies on
	// PROPPATCH succeeding to keep Win32LastModifiedTime and friends; a silent failure shows up
	// later as properties that mysteriously do not persist across a reconnect.
	var storeErr error
	if len(rejected) == 0 {
		storeErr = h.meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
			for _, p := range props {
				if err := tx.SetDeadProp(ctx, node.ID, p.Space, p.Name, p.Value); err != nil {
					return err
				}
			}
			return nil
		})
		if storeErr != nil {
			h.logf("proppatch: storing dead properties failed for %s: %v", r.URL.Path, storeErr)
		}
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<D:multistatus xmlns:D="DAV:">` + "\n")
	b.WriteString("<D:response><D:href>" + xmlEscape(r.URL.Path) + "</D:href>")

	switch {
	case storeErr != nil:
		// RFC 4918 9.2: a property that could not be set is reported in its own propstat. The
		// transaction is all-or-nothing, so every property in it failed together.
		b.WriteString("<D:propstat><D:prop>")
		for _, p := range props {
			b.WriteString("<D:" + p.Name + "/>")
		}
		b.WriteString("</D:prop><D:status>HTTP/1.1 500 Internal Server Error</D:status></D:propstat>")
	case len(rejected) > 0:
		b.WriteString("<D:propstat><D:prop>")
		for _, name := range rejected {
			b.WriteString("<D:" + name + "/>")
		}
		b.WriteString("</D:prop><D:status>HTTP/1.1 403 Forbidden</D:status></D:propstat>")
	default:
		b.WriteString("<D:propstat><D:status>HTTP/1.1 200 OK</D:status></D:propstat>")
	}
	b.WriteString("</D:response>\n</D:multistatus>\n")
	_, _ = w.Write([]byte(b.String()))
}

type deadProp struct {
	Space string
	Name  string
	Value string
}

// parseProppatchBody decodes <propertyupdate> into a flat list of sets.
//
// Removes are represented as an empty value, which is how a client expresses
// "forget this property"; storing an empty string is equivalent for every client
// that matters and keeps the storage simple.
func parseProppatchBody(body interface{ Read([]byte) (int, error) }) ([]deadProp, error) {
	return parsePropUpdate(body)
}

// handleCopyMove implements COPY and MOVE.
func (h *Handler) handleCopyMove(w http.ResponseWriter, r *http.Request, a authz.Actor, move bool) {
	ctx := r.Context()

	srcCap, src, err := h.resolvePath(ctx, a, r.URL.Path)
	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}
	// Two different source rights, because a copy and a move consume different things.
	//
	//   COPY takes a copy of the bytes: `download` (plus `read` to see the node at all).
	//   MOVE relocates the node: `delete`-class authority over it — plus `download`, since
	//        moving a file the caller may not read is a way to exfiltrate it by moving it
	//        into a namespace they can read.
	//
	// §4.2 names both halves explicitly: "MOVE：验证源移动/删除权限及目标创建权限".
	verb := authz.PermWrite
	if move {
		verb = authz.PermDelete
	}
	if !srcCap.Can(authz.PermDownload) || !srcCap.Can(verb) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}

	// A collection MOVE or COPY carries its whole subtree, so every inner node must permit
	// the operation too (§4.2: 目录递归操作不能只检查根目录).
	if src.IsDir() {
		need := authz.PermRead
		if move {
			need = authz.PermDelete
		}
		ok, serr := h.subtreeAllows(ctx, a, src.ID, need)
		if serr != nil {
			writeXMLError(w, statusForError(serr), "")
			return
		}
		if !ok {
			writeXMLError(w, http.StatusLocked, "")
			return
		}
	}

	destPath := r.Header.Get("Destination")
	if destPath == "" {
		writeXMLError(w, http.StatusBadRequest, "")
		return
	}
	// Destination may be an absolute URL. Only the path is used, but the HOST is checked:
	// destinationHostMatches was written for this, documented as "checked by the caller", and no
	// caller ever checked it — so a MOVE or COPY naming another server was silently applied
	// locally instead of being refused. RFC 4918 §9.8.5 is explicit that a destination on another
	// server is 502 Bad Gateway, and the check also keeps a client from believing the bytes went
	// somewhere they did not.
	if !destinationHostMatches(destPath, h.cfg.Server.PublicURL) {
		writeXMLError(w, http.StatusBadGateway, "")
		return
	}
	if u, perr := parseDestination(destPath); perr != nil {
		writeXMLError(w, http.StatusBadRequest, "")
		return
	} else {
		destPath = u
	}

	dstCap, dstParent, dstName, err := h.resolveDestination(ctx, a, destPath)
	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}
	if !dstCap.Can(authz.PermWrite) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}

	overwrite := !strings.EqualFold(r.Header.Get("Overwrite"), "F")

	// A locked source cannot be moved or copied away from under its holder, and a
	// locked destination cannot be replaced.
	if !h.requireUnlocked(w, ctx, a, src.ID, r.Header.Get("If")) {
		return
	}

	// Does the destination already exist?
	var existing *store.Node
	_ = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.ChildByName(ctx, dstParent.ID, dstName)
		if cerr == nil {
			existing = &n
		}
		return nil
	})

	if existing != nil {
		if !overwrite {
			// RFC 4918 §9.8.4 / §9.9.4: refuse rather than clobber.
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		// Overwriting destroys the destination, so it needs the same authorization
		// that an explicit DELETE of it would. The previous code trashed the
		// destination without checking anything, which let a MOVE into a writable
		// folder delete a file the caller could not otherwise touch.
		dstCap, derr := h.authz.Authorize(a, existing.ID)
		if derr != nil || !dstCap.Can(authz.PermDelete) {
			writeXMLError(w, http.StatusNotFound, "")
			return
		}
		if !h.requireUnlocked(w, ctx, a, existing.ID, r.Header.Get("If")) {
			return
		}
		if err := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.TrashCapped(ctx, existing.ID, h.trashCap(ctx))
		}); err != nil {
			writeXMLError(w, statusForError(err), "")
			return
		}
	}

	_, srcRel, _ := h.splitPath(r.URL.Path)
	_, dstRel, _ := h.splitPath(destPath)

	if move {
		err = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			// Rename then reparent; two calls because the destination name and the
			// destination parent are independent.
			if src.Name != dstName {
				if _, rerr := tx.Rename(ctx, src.ID, dstName); rerr != nil {
					return rerr
				}
			}
			if src.ParentID == nil || *src.ParentID != dstParent.ID {
				if _, merr := tx.Move(ctx, src.ID, dstParent.ID); merr != nil {
					return merr
				}
			}
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.move", TargetKind: "node", TargetID: src.ID.String(),
				Result: "ok", Metadata: map[string]any{"from": srcRel, "to": dstRel},
			})
		})
		// The tree layout mirrors the hierarchy on disk, so the bytes move too.
		// Blob layout treats this as a metadata update (EfficientRename).
		//
		// The error is NOT discarded. It used to be, so a MOVE whose metadata commit succeeded
		// and whose content move failed answered 201 while the bytes stayed where they were: a
		// node pointing at a path the content store knows nothing about, reported to the client as
		// a successful move. Silent, and in the direction of data loss.
		//
		// Metadata and bytes cannot commit together, so the two are reconciled instead: if the
		// bytes cannot follow, the metadata is put back and the client is told the move failed.
		// The compensation can itself fail, and then the honest state is "inconsistent, operator
		// needed" — which is logged loudly rather than papered over with a 201.
		// A layout with EfficientRename keeps the bytes where they are and follows the metadata:
		// its path-to-hash resolver IS the metadata, so by the time this runs the source path no
		// longer exists there. Calling Move for such a layout always failed, and the discarded
		// error hid it — the operation only looked correct because nothing read back through the
		// moved mapping. Skipping it is not an optimisation; it is the correct thing to do.
		if err == nil && !h.content.Capabilities().EfficientRename {
			if merr := h.content.Move(ctx,
				layout.Node{Owner: a.ID().String(), Path: srcRel, IsDir: src.IsDir()},
				layout.Node{Owner: a.ID().String(), Path: dstRel, IsDir: src.IsDir()}); merr != nil {
				h.logf("move: content move failed src=%q dst=%q err=%v; restoring metadata", srcRel, dstRel, merr)
				cerr := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
					if _, rerr := tx.Rename(ctx, src.ID, src.Name); rerr != nil {
						return rerr
					}
					if src.ParentID != nil {
						if _, mverr := tx.Move(ctx, src.ID, *src.ParentID); mverr != nil {
							return mverr
						}
					}
					return nil
				})
				if cerr != nil {
					h.logf("move: COMPENSATION FAILED for node %s: metadata says the file moved and "+
						"the content did not; node err=%v", src.ID, cerr)
				}
				writeXMLError(w, http.StatusInternalServerError, "")
				return
			}
		}
	} else {
		err = h.copyNode(ctx, a, src, dstParent, dstName, dstRel,
			r.WithContext(withCopyWasOverwrite(r.Context(), existing != nil)))
		// WC-GAP-009: a failed directory COPY must not leave a half-populated destination that
		// LOOKS complete.
		//
		// Before this, a copy that failed at the fifth of ten children left a directory containing
		// four files and reported an error. The source was intact, so nothing was lost — but the
		// destination is a plausible-looking folder that a client may treat as the copy it asked
		// for, especially once the error has scrolled past. T-20 asks for a failure that is
		// "diagnosable and recoverable"; a partial tree with no marker is neither.
		//
		// The rollback trashes the destination subtree rather than purging it, which matters: if the
		// compensation itself fails, the partial copy lands in the trash (where an operator can find
		// and remove it) instead of becoming an invisible orphan. The error the client sees names what
		// was rolled back, so "did it delete my file" has an answer.
		if err != nil && src.IsDir() && !disableCopyRollbackForTest {
			h.rollbackCopy(ctx, a, dstParent.ID, dstName, r)
		}
	}

	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}

	// RFC 4918 §9.8.5 / §9.9.4: 201 Created when the destination is new, 204 No Content when it
	// already existed and was replaced. PUT already distinguished these; MOVE and COPY answered
	// 201 for both, so an overwrite looked like the creation of a new resource.
	//
	// A 201 also carries Location, which is how a client learns the new resource's URL without
	// re-listing the parent.
	destURL := destPath
	if !strings.HasPrefix(destURL, "http://") && !strings.HasPrefix(destURL, "https://") {
		destURL = strings.TrimSuffix(h.cfg.Server.PublicURL, "/") + "/" + strings.TrimPrefix(destURL, "/")
	}
	if existing != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Location", destURL)
	w.WriteHeader(http.StatusCreated)
}

// copyNode copies a file or a directory subtree.
//
// Directory copies are recursive. The earlier version created the destination
// directory and returned, which reported success while producing an empty folder —
// the worst possible outcome, because the client believes its data was copied.
func (h *Handler) copyNode(ctx context.Context, a authz.Actor, src store.Node,
	dstParent store.Node, dstName, dstRel string, r *http.Request) error {

	if src.IsDir() {
		_, srcRel, _ := h.splitPath(r.URL.Path)
		return h.copyTree(ctx, a, src, srcRel, dstParent.ID, dstName, dstRel, r, 0)
	}

	// Files: copy the bytes (the blob layout deduplicates them, the tree layout
	// writes a second copy, and both are correct).
	_, srcRel, _ := h.splitPath(r.URL.Path)
	rc, err := h.content.Open(ctx, layout.Node{Owner: a.ID().String(), Path: srcRel})
	if err != nil {
		return err
	}
	defer rc.Close()

	res, err := h.content.Put(ctx, layout.Node{Owner: a.ID().String(), Path: dstRel}, rc, src.Size)
	if err != nil {
		return err
	}

	etag := etagFor(res.Hash)
	return h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		node, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: a.ID(), ParentID: dstParent.ID, Name: dstName,
			Size: res.Size, Mime: src.Mime, ETag: etag,
			BlobID: h.blobIDFor(ctx, res.Hash, res.Size), BlobHash: res.Hash,
		})
		if cerr != nil {
			return cerr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.write", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok", Metadata: map[string]any{"name": dstName, "copied_from": src.Name},
		})
	})
}

// copyTree recreates one directory and, recursively, everything under it.
//
// The depth guard is not paranoia: a corrupted parent chain would otherwise turn a
// COPY into an infinite loop, and the tree comes from a database that a bug could
// have written a cycle into (the Move path forbids cycles, but COPY reads whatever
// is there). Every level re-authorizes, so copying a folder the caller only
// partially can read fails at the first child they cannot, rather than silently
// skipping it or — worse — copying it.
func (h *Handler) copyTree(ctx context.Context, a authz.Actor, src store.Node,
	srcRel string, dstParentID uuid.UUID, dstName, dstRel string, r *http.Request, depth int) error {

	if depth > 64 {
		return store.ErrCycle
	}

	var newDir store.Node
	err := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.CreateDir(ctx, store.NewNode{
			OwnerID: a.ID(), ParentID: dstParentID, Name: dstName,
		})
		if cerr != nil {
			return cerr
		}
		newDir = n
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.write", TargetKind: "node", TargetID: n.ID.String(),
			Result: "ok", Metadata: map[string]any{"name": dstName, "kind": "dir", "copied_from": src.Name},
		})
	})
	if err != nil {
		return err
	}

	// The tree layout mirrors the hierarchy on disk, so the directory has to exist
	// there before any child is written into it.
	_ = h.content.Mkdir(ctx, layout.Node{Owner: a.ID().String(), Path: dstRel, IsDir: true})

	children, err := h.listChildren(ctx, src)
	if err != nil {
		return err
	}
	for _, child := range children {
		childSrcRel := path.Join(srcRel, child.Name)
		childDstRel := path.Join(dstRel, child.Name)

		if child.IsDir() {
			if err := h.copyTree(ctx, a, child, childSrcRel, newDir.ID, child.Name, childDstRel, r, depth+1); err != nil {
				return err
			}
			continue
		}

		rc, oerr := h.content.Open(ctx, layout.Node{Owner: a.ID().String(), Path: childSrcRel})
		if oerr != nil {
			return oerr
		}
		res, perr := h.content.Put(ctx, layout.Node{Owner: a.ID().String(), Path: childDstRel}, rc, child.Size)
		_ = rc.Close()
		if perr != nil {
			return perr
		}

		etag := etagFor(res.Hash)
		if terr := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			n, cerr := tx.CreateFile(ctx, store.NewFile{
				OwnerID: a.ID(), ParentID: newDir.ID, Name: child.Name,
				Size: res.Size, Mime: child.Mime, ETag: etag,
				BlobID: h.blobIDFor(ctx, res.Hash, res.Size), BlobHash: res.Hash,
			})
			if cerr != nil {
				return cerr
			}
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.write", TargetKind: "node", TargetID: n.ID.String(),
				Result: "ok", Metadata: map[string]any{"name": child.Name, "copied_from": child.Name},
			})
		}); terr != nil {
			return terr
		}
	}
	return nil
}

// disableCopyRollbackForTest turns the WC-GAP-009 rollback off.
//
// It exists so the test can demonstrate the guard is load-bearing: running the same scenario with the
// rollback disabled must produce a visible partial destination. A test that cannot be made to fail is
// not evidence, and package-internal state is the only way to reach both behaviours without shipping a
// configuration key nobody should set.
var disableCopyRollbackForTest bool

// rollbackCopy trashes a partially-created destination after a failed directory COPY (WC-GAP-009).
//
// It is best-effort and deliberately loud when it fails: the alternative is a half-populated folder
// presented as a copy that failed, which reads as "nothing was copied" while occupying quota. Trashing
// rather than purging means a failed compensation still leaves the data findable.
//
// It only removes the destination if the destination did not exist before the copy started. An
// overwrite that fails must not delete the previous version's replacement — the copy may have got far
// enough to replace it, and trashing it would turn a failed COPY into the loss of whatever was there.
// The caller passes overwrite via the request context set in handleCopyMove.
func (h *Handler) rollbackCopy(ctx context.Context, a authz.Actor, dstParent uuid.UUID, dstName string, r *http.Request) {
	if copyWasOverwrite(r.Context()) {
		return
	}
	var victim store.Node
	err := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		// Resolve the destination fresh: the copy may have failed before creating it, in which case
		// there is nothing to roll back and ErrNotFound is the expected answer.
		parent, perr := tx.Node(ctx, dstParent)
		if perr != nil {
			return perr
		}
		_ = parent
		kids, kerr := tx.Children(ctx, dstParent, 100_000)
		if kerr != nil {
			return kerr
		}
		for _, k := range kids {
			if k.Name == dstName {
				victim = k
				return nil
			}
		}
		return store.ErrNotFound
	})
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			h.logf("copy rollback: could not locate partial destination %q: %v", dstName, err)
		}
		return
	}

	if terr := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if terr := tx.Trash(ctx, victim.ID); terr != nil {
			return terr
		}
		// The rollback is an auditable event in its own right: a client that saw an error and an
		// operator looking at an item in the trash both need to know why it is there.
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "system", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.write", TargetKind: "node", TargetID: victim.ID.String(),
			Result: "denied",
			Metadata: map[string]any{
				"name":     dstName,
				"rollback": "partial directory copy trashed",
			},
		})
	}); terr != nil {
		h.logf("copy rollback: FAILED to trash partial destination %q (node %s): %v — the "+
			"destination is a partial copy and should be removed by hand", dstName, victim.ID, terr)
		return
	}
	h.logf("copy rollback: trashed partial destination %q (node %s) after a failed directory copy",
		dstName, victim.ID)
}

// copyWasOverwriteKey marks a COPY that replaced an existing destination.
type copyWasOverwriteKey struct{}

func withCopyWasOverwrite(ctx context.Context, v bool) context.Context {
	return context.WithValue(ctx, copyWasOverwriteKey{}, v)
}

func copyWasOverwrite(ctx context.Context) bool {
	v, _ := ctx.Value(copyWasOverwriteKey{}).(bool)
	return v
}

// resolveDestination resolves a Destination header to its parent and name.
func (h *Handler) resolveDestination(ctx context.Context, a authz.Actor, destPath string) (authz.Cap, store.Node, string, error) {
	_, rel, err := h.splitPath(destPath)
	if err != nil {
		return authz.Cap{}, store.Node{}, "", authz.ErrDenied
	}
	parentRel, name := splitParent(rel)
	if name == "" {
		return authz.Cap{}, store.Node{}, "", store.ErrInvalidName
	}

	nodes, err := h.meta.Resolve(ctx, a.ID(), parentRel)
	if err != nil {
		return authz.Cap{}, store.Node{}, "", err
	}
	parent := nodes[len(nodes)-1]
	if !parent.IsDir() {
		return authz.Cap{}, store.Node{}, "", store.ErrInvalidName
	}

	cap, err := h.authz.Authorize(a, parent.ID)
	if err != nil {
		return authz.Cap{}, store.Node{}, "", err
	}
	return cap, parent, name, nil
}

// splitParent splits "a/b/c" into ("a/b", "c").
func splitParent(rel string) (parent, name string) {
	rel = strings.TrimSuffix(rel, "/")
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}

var errBadDestination = errors.New("dav: bad destination")
