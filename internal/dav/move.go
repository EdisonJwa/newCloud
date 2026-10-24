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
	"github.com/EdisonJwa/newCloud/internal/dircommit"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/swap"
	"github.com/EdisonJwa/newCloud/internal/treewalk"
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
	//   COPY takes a copy of the bytes: `read` (to see the node at all) plus `download`. It does
	//        NOT need `write` on the source: copying something the caller may only read is the
	//        ordinary use of a read-only grant, and requiring write made a read-only grantee
	//        unable to copy bytes they are explicitly allowed to fetch — so `download` on the
	//        source was a permission with no route that could exercise it (P1 finding in the
	//        2026-10-23 re-review).
	//
	//   MOVE relocates the node: `delete`-class authority over it — plus `download`, since
	//        moving a file the caller may not read is a way to exfiltrate it by moving it
	//        into a namespace they can read.
	//
	// §4.2 names both halves explicitly: "MOVE：验证源移动/删除权限及目标创建权限", and for COPY
	// "源文件读取权限与目标路径创建权限".
	if !sourcePermits(srcCap.Perms(), move) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}

	// A collection MOVE or COPY carries its whole subtree, so every inner node must permit
	// the operation too (§4.2: 目录递归操作不能只检查根目录).
	if src.IsDir() {
		ok, serr := h.subtreeAllows(ctx, a, src.ID, subtreeSourcePerm(move))
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

	// Locks on the collections a COPY or MOVE changes.
	//
	// Both requests add a member to the destination's collection, and MOVE removes one from
	// the source's. A Depth: 0 lock protects exactly that membership, so the handler has to
	// consult it — before this, a client could lock a folder and another client could still
	// COPY a file into it or MOVE one out with 201/204 (2026-10-24 review, H2).
	//
	// The depth split is the same one PUT makes: when the destination already exists the
	// request changes that member's CONTENT, so only a Depth: infinity ancestor reaches it;
	// when it does not exist the request changes the parent's membership, which a Depth: 0
	// lock covers.
	if existing != nil {
		if h.lockAncestorsInfinityOnly(ctx, a, dstParent.ID, r.Header.Get("If")) {
			writeXMLError(w, http.StatusLocked, "lock-token-submitted")
			return
		}
	} else if h.lockAncestors(ctx, a, dstParent.ID, r.Header.Get("If")) {
		writeXMLError(w, http.StatusLocked, "lock-token-submitted")
		return
	}
	if move && src.ParentID != nil {
		if h.lockAncestors(ctx, a, *src.ParentID, r.Header.Get("If")) {
			writeXMLError(w, http.StatusLocked, "lock-token-submitted")
			return
		}
	}

	// A MOVE of a collection relocates every descendant, and a COPY of one reads every
	// descendant's bytes. A lock on a child is therefore at stake in both, and the holder
	// may only proceed by submitting its token.
	if src.IsDir() {
		var held []store.Lock
		if lerr := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			l, err := tx.LocksInSubtree(ctx, src.ID)
			held = l
			return err
		}); lerr != nil {
			writeXMLError(w, statusForError(lerr), "")
			return
		}
		for _, l := range held {
			// The source node's own lock was already checked above; this covers children.
			if l.NodeID == src.ID {
				continue
			}
			if lockBlocksFor(l, a, r.Header.Get("If")) {
				writeXMLError(w, http.StatusLocked, "lock-token-submitted")
				return
			}
		}
	}

	// A destination that IS the source is not an overwrite, and obeying the request as one destroys
	// the very resource it names. The fourth review (2026-10-23) reproduced it: MOVE onto its own URI
	// found "the destination exists", trashed it — the source node — then performed a move that was a
	// no-op, answered 204, and the file was in the bin (P0-DAV-001).
	//
	// The check is by IDENTITY, after resolution, not by comparing the two strings the client sent:
	// "keep.txt" and "keep%2Etxt" are different strings and the same node, and a name the store folds
	// to one key (case, Unicode normalisation) is the same destination however it is spelled.
	if existing != nil && existing.ID == src.ID {
		if !move || dstName == src.Name {
			// RFC 4918 §9.9.4 / §9.8.5 permits refusing a MOVE or COPY whose destination is the
			// source. There is nothing to do and nothing to destroy.
			writeXMLError(w, http.StatusForbidden, "")
			return
		}
		// A different SPELLING of the node's own name (a case-only rename, which Windows Explorer
		// sends as a MOVE) is a legitimate rename, not an overwrite: relocating the node onto its own
		// name index entry replaces nothing. No destination is destroyed, so no DELETE authority over
		// a "destination" is required — the caller's MOVE rights over the source are the whole rule.
		_, srcRel, _ := h.splitPath(r.URL.Path)
		_, dstRel, _ := h.splitPath(destPath)
		if _, rerr := swap.Relocate(ctx,
			swap.Deps{Meta: h.meta, Content: h.content, Log: h.log},
			swap.Relocation{
				Node: src, FromRel: srcRel, ParentID: dstParent.ID, Name: dstName, ToRel: dstRel,
				Audit: func(ctx context.Context, tx store.Tx, moved store.Node) error {
					return tx.Audit(ctx, store.AuditEntry{
						ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
						ActorIP: clientIP(r), UserAgent: r.UserAgent(),
						Action: "file.move", TargetKind: "node", TargetID: moved.ID.String(),
						Result: "ok", Metadata: map[string]any{"from": srcRel, "to": dstRel},
					})
				},
			}); rerr != nil {
			writeXMLError(w, statusForError(rerr), "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// A collection cannot be moved or copied into its own subtree (RFC 4918 §9.9.4: 409 Conflict).
	//
	// This is checked HERE, before anything is trashed or renamed, because the check used to be the
	// metadata Move's ErrCycle — discovered in a later transaction than the one that had already
	// trashed the existing destination. The client saw 409 and lost the folder and its files
	// (P0-DAV-002). A refusal must have no side effects, so the whole question is answered first.
	if src.IsDir() && h.insideSubtree(ctx, src.ID, dstParent.ID) {
		writeXMLError(w, http.StatusConflict, "")
		return
	}

	_, srcRel, _ := h.splitPath(r.URL.Path)
	_, dstRel, _ := h.splitPath(destPath)

	// A directory COPY is resolved to completion BEFORE anything is created, staged or trashed
	// (P0-DAV-COPY-001/002).
	//
	// This used to recurse through h.listChildren, which applies limits.max_profind_children — the
	// page size of PROPFIND — and reports nothing when it truncates. With that limit at 1, COPY of a
	// two-file folder answered 201 with one file copied, and an overwrite answered 204 while replacing
	// a complete destination with an incomplete copy. A transfer must either cover the tree or fail
	// before it starts; "most of it, reported as success" is the one outcome a client cannot detect.
	//
	// The same completeness-proven walk the JSON batch and archive use is therefore the only source of
	// the tree, and the resolution happens here rather than inside the copy so a refusal has no side
	// effects at all.
	var copyPlan *treewalk.Tree
	if !move && src.IsDir() {
		plan, perr := treewalk.Resolve(ctx, h.meta, src, srcRel, copyWalkBudget())
		if perr != nil {
			h.logf("copy refused: cannot prove the source subtree complete path=%q err=%v", srcRel, perr)
			writeXMLError(w, statusForCopyWalkError(perr), "")
			return
		}
		copyPlan = plan
	}

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
		if !move {
			// A COPY over an existing destination does NOT trash it (NC-RR-005): the copy is staged
			// under a reserved sibling name and swapped in only once it has fully succeeded.
			if !disableCopyOverwritePreserveForTest {
				if cerr := h.copyOverExisting(w, r, a, src, copyPlan, dstParent, dstName, *existing, destPath); cerr != nil {
					writeXMLError(w, statusForError(cerr), "")
				}
				return
			}
			// The guard is switched off to prove it is load-bearing; the legacy path trashes the
			// destination first and reproduces the defect.
			if terr := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				return tx.TrashCapped(ctx, existing.ID, h.trashCap(ctx))
			}); terr != nil {
				writeXMLError(w, statusForError(terr), "")
				return
			}
		} else {
			// A MOVE overwrite uses the same borrowed swap the JSON API does. It is "borrowed"
			// because the stage is the caller's own source node, relocated to a reserved sibling
			// name: a rollback has to RETURN it, and the displaced destination has to keep its bytes
			// so the trash can restore them (P0-DAV-003 — the old code moved the source's bytes over
			// the destination's, leaving a trash entry with nothing to restore).
			if cerr := h.moveOverExisting(w, r, a, src, dstParent, dstName, *existing, destPath); cerr != nil {
				writeXMLError(w, statusForError(cerr), "")
			}
			return
		}
	}

	if move {
		if _, merr := swap.Relocate(ctx,
			swap.Deps{Meta: h.meta, Content: h.content, Log: h.log},
			swap.Relocation{
				Node: src, FromRel: srcRel, ParentID: dstParent.ID, Name: dstName, ToRel: dstRel,
				Audit: func(ctx context.Context, tx store.Tx, moved store.Node) error {
					return tx.Audit(ctx, store.AuditEntry{
						ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
						ActorIP: clientIP(r), UserAgent: r.UserAgent(),
						Action: "file.move", TargetKind: "node", TargetID: moved.ID.String(),
						Result: "ok", Metadata: map[string]any{"from": srcRel, "to": dstRel},
					})
				},
			}); merr != nil {
			// swap.Relocate puts the metadata back when the bytes cannot follow, so a failure here
			// means the move did not happen — which is what the client is told.
			writeXMLError(w, statusForError(merr), "")
			return
		}
	} else {
		// createdIDs records every node THIS request committed, in creation order. The
		// rollback below is driven by identity, not by a fresh lookup of the destination
		// name: after a conflict with a concurrent MKCOL/COPY, the name belongs to another
		// operation's already-successful node and trashing "whatever has that name" would
		// revoke a success the client was told about (R8-DAV-001).
		var createdIDs []createdNode
		err := h.copyNode(ctx, a, src, copyPlan, dstParent, dstName, dstRel,
			r.WithContext(withCopyWasOverwrite(r.Context(), existing != nil)), &createdIDs)
		// WC-GAP-009: a failed directory COPY must not leave a half-populated destination that
		// LOOKS complete.
		//
		// Before this, a copy that failed at the fifth of ten children left a directory containing
		// four files and reported an error. The source was intact, so nothing was lost — but the
		// destination is a plausible-looking folder that a client may treat as the copy it asked
		// for, especially once the error has scrolled past. T-20 asks for a failure that is
		// "diagnosable and recoverable"; a partial tree with no marker is neither.
		//
		// The rollback trashes the nodes this COPY created, in reverse creation order, and never
		// a node it did not create. Trashing rather than purging means a failed compensation is
		// findable in the trash instead of becoming an invisible orphan.
		if err != nil && src.IsDir() && !disableCopyRollbackForTest {
			h.rollbackCopy(ctx, a, createdIDs, dstName, r)
		}
		if err != nil {
			writeXMLError(w, statusForError(err), "")
			return
		}
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

// insideSubtree reports whether nodeID is rootID or lies below it.
//
// The same question the JSON API asks before moving a folder into itself, asked here for the same
// reason: the answer decides whether the operation can be performed at all, and it has to be known
// BEFORE the destination is touched. Relying on the metadata layer's ErrCycle made the check later
// than the trash it was supposed to prevent (P0-DAV-002).
func (h *Handler) insideSubtree(ctx context.Context, rootID, nodeID [16]byte) bool {
	cur := nodeID
	for depth := 0; depth < 256; depth++ {
		if cur == rootID {
			return true
		}
		var n store.Node
		if err := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			got, gerr := tx.Node(ctx, cur)
			n = got
			return gerr
		}); err != nil || n.ParentID == nil {
			return false
		}
		cur = *n.ParentID
	}
	// An ancestor chain longer than this is a corrupted tree, not a cycle we can rule out.
	return true
}

// moveOverExisting performs a MOVE whose destination already exists.
//
// The shape is the batch API's borrowed swap, because it is the same operation: the source node is
// relocated to a reserved sibling name (the "stage"), the destination's bytes are moved aside under
// the backup name, the source is installed at the destination name, and the displaced destination
// goes to the trash — all in one metadata transaction. Borrowed means exactly what it means there:
// a failure returns the source to where it came from instead of deleting it.
//
// What the old code did instead — trash the destination, then move the source onto its path — lost
// the destination's bytes on the tree layout, because a trashed node keeps its metadata and loses its
// physical path to the next rename. Restoring it afterwards gave a name with no content to read
// (P0-DAV-003).
func (h *Handler) moveOverExisting(w http.ResponseWriter, r *http.Request, a authz.Actor,
	src, dstParent store.Node, dstName string, existing store.Node, destPath string) error {

	ctx := r.Context()
	_, srcRel, _ := h.splitPath(r.URL.Path)
	_, dstRel, _ := h.splitPath(destPath)
	parentRel := path.Dir(dstRel)
	if parentRel == "." {
		parentRel = ""
	}

	deps := swap.Deps{Meta: h.meta, Content: h.content, Log: h.log, TrashCap: h.trashCap(ctx)}

	_, err := swap.Install(ctx, deps, swap.Request{
		Owner: a.ID().String(), Parent: dstParent, ParentRel: parentRel, DstName: dstName,
		Existing: existing, StagedIsDir: src.IsDir(),
		// Borrowed: the staged node is the caller's own source, not a copy this operation built.
		Replacement: swap.Borrowed(
			func(ctx context.Context, stagedName string) (store.Node, error) {
				return swap.Relocate(ctx, deps, swap.Relocation{
					Node: src, FromRel: srcRel, ParentID: dstParent.ID, Name: stagedName,
					ToRel: path.Join(parentRel, stagedName),
					Audit: func(ctx context.Context, tx store.Tx, moved store.Node) error {
						return tx.Audit(ctx, store.AuditEntry{
							ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
							ActorIP: clientIP(r), UserAgent: r.UserAgent(),
							Action: "file.move", TargetKind: "node", TargetID: moved.ID.String(),
							Result: "ok", Metadata: map[string]any{
								"from": srcRel, "to": dstRel, "staged": true,
							},
						})
					},
				})
			},
			func(ctx context.Context, staged store.Node) error {
				if src.ParentID == nil {
					return errors.New("dav: the source has no parent to return to")
				}
				_, rerr := swap.Relocate(ctx, deps, swap.Relocation{
					Node: staged, FromRel: path.Join(parentRel, staged.Name),
					ParentID: *src.ParentID, Name: src.Name, ToRel: srcRel,
					Audit: func(ctx context.Context, tx store.Tx, moved store.Node) error {
						return tx.Audit(ctx, store.AuditEntry{
							ActorKind: "system", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
							ActorIP: clientIP(r), UserAgent: r.UserAgent(),
							Action: "file.move", TargetKind: "node", TargetID: moved.ID.String(),
							Result: "denied",
							Metadata: map[string]any{
								"name": src.Name, "rollback": "MOVE overwrite that did not commit",
							},
						})
					},
				})
				return rerr
			},
		),
		AuditRows: func(ctx context.Context, tx store.Tx, installed, aside store.Node) error {
			if aerr := tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.move", TargetKind: "node", TargetID: installed.ID.String(),
				Result: "ok", Metadata: map[string]any{
					"from": srcRel, "to": dstRel, "replaced": aside.Name,
					"replaced_node": aside.ID.String(),
				},
			}); aerr != nil {
				return aerr
			}
			// The displaced destination is a deletion the owner should see in the log, exactly as
			// the batch API records it.
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.delete", TargetKind: "node", TargetID: aside.ID.String(),
				Result: "ok", Metadata: map[string]any{
					"name": dstName, "reason": "replaced by a MOVE",
					"replaced_by": installed.ID.String(),
				},
			})
		},
		ActorID: a.Ptr(), ActorEmail: a.Email(), ActorIP: clientIP(r), UserAgent: r.UserAgent(),
	})
	if err != nil {
		return err
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

// createdNode is a node this COPY committed, together with the state it had at that moment.
//
// The ID alone proves who created the node, not who owns it NOW: another request can overwrite
// the file (a successful PUT), rename it, or move its directory since. The rollback must not
// undo an operation the client was told succeeded, so it compares the node's current location
// and content fingerprint with this snapshot before removing anything.
type createdNode struct {
	ID         uuid.UUID
	ParentID   uuid.UUID
	Name       string
	ETag       string
	Size       int64
	ModifiedAt time.Time
	IsDir      bool
}

// copyNode copies a file or a directory subtree.
//
// A directory is copied from plan, the completeness-proven tree the caller resolved BEFORE the
// destination (or its staging name) existed. plan may be nil only for a file source; a directory
// without a plan is a programming error rather than a reason to fall back to a truncated walk.
func (h *Handler) copyNode(ctx context.Context, a authz.Actor, src store.Node, plan *treewalk.Tree,
	dstParent store.Node, dstName, dstRel string, r *http.Request, created *[]createdNode) error {

	if src.IsDir() {
		if plan == nil {
			return errors.New("dav: a directory copy was given no resolved tree")
		}
		return h.copyResolved(ctx, a, plan, dstParent.ID, dstName, dstRel, r, created)
	}

	// Files: copy the bytes (the blob layout deduplicates them, the tree layout
	// writes a second copy, and both are correct).
	_, srcRel, _ := h.splitPath(r.URL.Path)
	rc, err := h.content.Open(ctx, layout.Node{Owner: a.ID().String(), Path: srcRel})
	if err != nil {
		return err
	}
	defer rc.Close()

	owner := a.ID().String()
	finalNode := layout.Node{Owner: owner, Path: dstRel, IsDir: false}

	// Where the content address IS the path (the tree layout), the copy must not publish its
	// bytes at the destination before the metadata row is ours.
	//
	// The old order wrote dstRel first and created the row second, so a concurrent PUT that
	// had already returned 201/204 for that name had its bytes replaced by a copy that then
	// failed to publish — the winner's success was revoked with no error addressed to it
	// (2026-10-24 review, C3; internal/dav/phase0_red_test.go still pins it).
	//
	// The blob layout needs no staging: its path is a metadata mapping and its bytes are
	// content-addressed, so a losing copy leaves an orphan blob at worst and can never
	// displace what the winner serves.
	pathsFollowContent := !h.content.Capabilities().EfficientRename
	writeNode := finalNode
	var stageNode layout.Node
	if pathsFollowContent {
		stageNode = layout.Node{
			Owner: owner,
			Path:  path.Join(path.Dir(dstRel), swap.StagedName(dstName)),
			IsDir: false,
		}
		writeNode = stageNode
	}

	res, err := h.content.Put(ctx, writeNode, rc, src.Size)
	if err != nil {
		return err
	}

	etag := etagFor(res.Hash)
	var made store.Node
	err = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		node, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: a.ID(), ParentID: dstParent.ID, Name: dstName,
			Size: res.Size, Mime: src.Mime, ETag: etag,
			BlobID: h.blobIDFor(ctx, res.Hash, res.Size), BlobHash: res.Hash,
		})
		if cerr != nil {
			return cerr
		}
		made = node
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.write", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok", Metadata: map[string]any{"name": dstName, "copied_from": src.Name},
		})
	})
	if err == nil && created != nil {
		*created = append(*created, createdNode{
			ID: made.ID, ParentID: dstParent.ID, Name: dstName,
			ETag: made.ETag, Size: made.Size, ModifiedAt: made.ModifiedAt,
		})
	}
	if err != nil {
		if pathsFollowContent {
			// Nothing was published, so the staged bytes are ours to remove. Best effort:
			// the reserved name is invisible to listings, and the sweep collects it.
			_ = h.content.Delete(ctx, stageNode)
		}
		return err
	}
	if !pathsFollowContent {
		return nil
	}

	// The row is ours, so the bytes may take the name.
	if mvErr := h.content.Move(ctx, stageNode, finalNode); mvErr != nil {
		_ = h.content.Delete(ctx, stageNode)
		// The row is live and its bytes are not where it says. Trashing it is the honest
		// compensation: the node appears in the bin rather than as a file that answers 500
		// forever, and a failed compensation is logged rather than silently swallowed.
		if terr := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, made.ID)
		}); terr != nil {
			h.logf("copy: metadata committed but the bytes could not be installed, and the "+
				"compensation failed; node=%s path=%q err=%v", made.ID, dstRel, terr)
		}
		return mvErr
	}
	return nil
}

// copyResolved recreates one directory and, recursively, everything under it from an already
// resolved tree.
//
// The tree is walked exactly as resolved, never re-listed: re-reading the source per directory is
// what allowed a page-limited listing to decide which files a "successful" COPY contained. The tree
// guarantees a depth the recursion can handle and a node count the caller already budgeted for, so
// there is no cycle to guard against here — Resolve refused anything it could not prove.
func (h *Handler) copyResolved(ctx context.Context, a authz.Actor, tn *treewalk.Tree,
	dstParentID uuid.UUID, dstName, dstRel string, r *http.Request, created *[]createdNode) error {

	var newDir store.Node
	// The physical directory exists before the node does. Ignoring the Mkdir result here was
	// NC-R6-001: COPY answered 201/204 for an empty directory that the tree layout never
	// created, and the metadata (and its success audit row) named a directory a backup or an
	// `ls` would not contain. A failure now stops the copy instead of being discarded.
	err := dircommit.Create(ctx, h.content,
		layout.Node{Owner: a.ID().String(), Path: dstRel, IsDir: true},
		func(ctx context.Context) error {
			return h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
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
					Result: "ok", Metadata: map[string]any{"name": dstName, "kind": "dir", "copied_from": tn.Node.Name},
				})
			})
		})
	if err != nil {
		return err
	}
	if created != nil {
		*created = append(*created, createdNode{
			ID: newDir.ID, ParentID: dstParentID, Name: dstName,
			ModifiedAt: newDir.ModifiedAt, IsDir: true,
		})
	}

	for _, child := range tn.Children {
		childDstRel := path.Join(dstRel, child.Node.Name)

		if child.Node.IsDir() {
			if err := h.copyResolved(ctx, a, child, newDir.ID, child.Node.Name, childDstRel, r, created); err != nil {
				return err
			}
			continue
		}

		rc, oerr := h.content.Open(ctx, layout.Node{Owner: a.ID().String(), Path: child.Rel})
		if oerr != nil {
			return oerr
		}
		res, perr := h.content.Put(ctx, layout.Node{Owner: a.ID().String(), Path: childDstRel}, rc, child.Node.Size)
		_ = rc.Close()
		if perr != nil {
			return perr
		}

		etag := etagFor(res.Hash)
		var made store.Node
		if terr := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			n, cerr := tx.CreateFile(ctx, store.NewFile{
				OwnerID: a.ID(), ParentID: newDir.ID, Name: child.Node.Name,
				Size: res.Size, Mime: child.Node.Mime, ETag: etag,
				BlobID: h.blobIDFor(ctx, res.Hash, res.Size), BlobHash: res.Hash,
			})
			if cerr != nil {
				return cerr
			}
			made = n
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.write", TargetKind: "node", TargetID: n.ID.String(),
				Result: "ok", Metadata: map[string]any{"name": child.Node.Name, "copied_from": child.Node.Name},
			})
		}); terr != nil {
			return terr
		}
		if created != nil {
			*created = append(*created, createdNode{
				ID: made.ID, ParentID: newDir.ID, Name: child.Node.Name,
				ETag: made.ETag, Size: made.Size, ModifiedAt: made.ModifiedAt,
			})
		}
	}
	return nil
}

// sourcePermits reports whether a permission set on the SOURCE is enough for the operation.
//
// It takes a Perm rather than an authz.Cap so the rule is testable from outside the authz package
// (Cap's fields are unexported precisely so a handler cannot fabricate one), and because the rule is
// about permissions, not about which node a checked capability happens to be bound to.
//
// The handler resolves paths within the caller's own namespace, which makes the cross-account COPY
// case unreachable through a DAV request: a grant that reaches another account's tree is consumed by
// the API's `/shared/{id}/content` route. The rule is still worth pinning on its own, because the
// previous version of it (requiring `write` on the source of a COPY) was wrong for exactly the
// read-only-grantee case §4.2 names, and a test that can only fail by constructing the whole
// cross-account topology would not have caught it either.
//
// COPY: read + download on the source — `read` to see the node, `download` to take the bytes.
// MOVE: download + delete on the source — `delete` because the source name is being consumed.
func sourcePermits(perms authz.Perm, move bool) bool {
	if perms&authz.PermDownload == 0 {
		return false
	}
	if move {
		return perms&authz.PermDelete != 0
	}
	return perms&authz.PermRead != 0
}

// subtreeSourcePerm is the per-inner-node right a recursive MOVE or COPY needs (NC-RR-005 §4.2).
//
// A MOVE consumes the subtree, so every inner node must permit deleting it. A COPY only reads, so
// `read` is the right bar there; demanding more would fail a tree the caller may legitimately copy.
func subtreeSourcePerm(move bool) authz.Perm {
	if move {
		return authz.PermDelete
	}
	return authz.PermRead
}

// copyOverExisting performs a COPY whose destination already exists, without destroying the old
// destination until the new one is complete.
//
// The shape is stage-then-swap. Two things about the ORDER are load-bearing, and the first version
// of this function got the second one wrong badly enough that COPY returned 500 and the user's
// destination vanished from the live tree:
//
//  1. Stage the replacement under a reserved sibling name. This is the expensive, failure-prone
//     part: every child is written while the old destination is still untouched at its real name.
//
//  2. For a layout whose bytes follow PATHS (the tree layout), move the physical bytes BEFORE the
//     metadata switch, and in the order that keeps both paths meaningful:
//     old  <dst>  -> <backup>     (frees the destination path, old bytes preserved)
//     new  <stage> -> <dst>       (the replacement is now where its name will say)
//     Doing it after the metadata rename, as the first version did, is guaranteed to fail: renaming
//     the metadata row does not move a directory, so the old tree still occupies the destination
//     path and the new tree's rename lands on a non-empty directory. A metadata-only layout (blob)
//     has no such step, because its paths ARE its metadata.
//
//  3. Switch the namespace in ONE metadata transaction: original -> backup name, staged -> the
//     destination name, and the displaced original to the trash. One transaction is what makes the
//     switch atomic for readers of the metadata, and it is also where the audit rows are written,
//     so a completed swap can never be missing from the log.
//
// Failure handling:
//
//   - stage failed or an early physical move failed: the destination was never touched, so the
//     staged tree is trashed and the error is returned.
//   - the swap transaction failed: the physical moves are reversed and the staged tree is trashed,
//     restoring the original at its own name with its own bytes.
//
// What is NOT claimed: atomicity across a process kill between the physical moves and the metadata
// transaction. No portable filesystem provides a multi-entry atomic swap, so the honest guarantee
// is "recoverable replacement": the original's bytes are always preserved under the backup name and
// the swap can be completed or undone by hand. `reconcileOverwriteArtifacts` detects and repairs the
// detectable cases at startup; docs/guides/limitations.md states the window.
func (h *Handler) copyOverExisting(w http.ResponseWriter, r *http.Request, a authz.Actor,
	src store.Node, plan *treewalk.Tree, dstParent store.Node, dstName string,
	existing store.Node, destPath string) error {

	ctx := r.Context()

	_, dstRel, _ := h.splitPath(destPath)
	parentRel := path.Dir(dstRel)
	if parentRel == "." {
		parentRel = ""
	}

	_, err := swap.Install(ctx, swap.Deps{
		Meta: h.meta, Content: h.content, Log: h.log, TrashCap: h.trashCap(ctx),
	}, swap.Request{
		Owner: a.ID().String(), Parent: dstParent, ParentRel: parentRel, DstName: dstName,
		Existing: existing, StagedIsDir: src.IsDir(),
		// A COPY's stage is a duplicate this operation built, so a failed install may destroy it.
		// Declaring that explicitly is what distinguishes it from a MOVE, whose stage is the
		// caller's own source node and must be returned rather than removed.
		Replacement: swap.Disposable(func(ctx context.Context, stagedName string) (store.Node, error) {
			// copyNode is reused rather than reimplemented, so the same resolved tree, audit entries
			// and error handling apply exactly as they do on the non-overwrite path.
			if cerr := h.copyNode(ctx, a, src, plan, dstParent, stagedName,
				path.Join(parentRel, stagedName),
				r.WithContext(withCopyWasOverwrite(ctx, false)), nil); cerr != nil {
				return store.Node{}, cerr
			}
			return h.childByName(ctx, dstParent.ID, stagedName)
		}),
		AuditRows: func(ctx context.Context, tx store.Tx, installed, aside store.Node) error {
			if aerr := tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.write", TargetKind: "node", TargetID: installed.ID.String(),
				Result: "ok", Metadata: map[string]any{
					"name": dstName, "copied_from": src.Name, "replaced": existing.Name,
					"replaced_node": existing.ID.String(),
				},
			}); aerr != nil {
				return aerr
			}
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.delete", TargetKind: "node", TargetID: aside.ID.String(),
				Result: "ok", Metadata: map[string]any{
					"name": dstName, "reason": "replaced by a COPY overwrite",
					"replaced_by": installed.ID.String(),
				},
			})
		},
		ActorID: a.Ptr(), ActorEmail: a.Email(), ActorIP: clientIP(r), UserAgent: r.UserAgent(),
	})
	if err != nil {
		return err
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

// childByName resolves a direct child of a parent by name.
func (h *Handler) childByName(ctx context.Context, parentID uuid.UUID, name string) (store.Node, error) {
	var out store.Node
	err := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.ChildByName(ctx, parentID, name)
		if cerr != nil {
			return cerr
		}
		out = n
		return nil
	})
	return out, err
}

// copyWalkBudgetForTest lowers the completeness budget for one COPY.
//
// The refusal path is otherwise unreachable without building a ten-thousand-node tree, and a guard
// that cannot be made to fail is not evidence. Package-internal, like the swap hooks below: no
// deployment can reach it, and no configuration key should exist for "copy only most of a folder".
var copyWalkBudgetForTest int

// copyWalkBudget is the node budget for one COPY's completeness proof.
//
// Deliberately independent of limits.max_profind_children: that setting sizes a PROPFIND response,
// and a display page must never decide whether a copy contained a file.
func copyWalkBudget() int {
	if copyWalkBudgetForTest > 0 {
		return copyWalkBudgetForTest
	}
	return treewalk.DefaultBudget
}

// statusForCopyWalkError maps a completeness refusal onto a DAV status.
//
// 413 is the honest answer: the request is well formed but the collection is larger than this server
// will transfer in one operation. 409 is for a tree whose parent links do not make a tree, which is
// not something a retry fixes. Everything else keeps the normal mapping (a store failure is a 500).
func statusForCopyWalkError(err error) int {
	switch {
	case errors.Is(err, treewalk.ErrTooLarge), errors.Is(err, treewalk.ErrTooDeep):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, treewalk.ErrInconsistent):
		return http.StatusConflict
	default:
		return statusForError(err)
	}
}

// disableCopyOverwritePreserveForTest turns the NC-RR-005 staged swap off.
//
// It exists for one reason: to prove the guard is load-bearing. Running the same failed-overwrite
// scenario with it set must reproduce the defect the review found (the destination's contents gone),
// which is the only way to show the new path is what preserves them rather than the quota simply
// having been generous. Package-internal state, so no deployment can reach it.
var disableCopyOverwritePreserveForTest bool

// disableCopyRollbackForTest turns the WC-GAP-009 rollback off.
//
// It exists so the test can demonstrate the guard is load-bearing: running the same scenario with the
// rollback disabled must produce a visible partial destination. A test that cannot be made to fail is
// not evidence, and package-internal state is the only way to reach both behaviours without shipping a
// configuration key nobody should set.
var disableCopyRollbackForTest bool

// rollbackCopy trashes the nodes a failed directory COPY created for itself, but only while they
// are still exactly where that COPY left them (WC-GAP-009, R8-DAV-001 and the follow-up).
//
// created is the ordered list of nodes THIS request committed, each with the location and content
// fingerprint it had at creation. Creation is not current ownership: another request may have
// overwritten the file (a successful PUT changes its ETag and mtime), renamed it, or moved a whole
// directory since. Removing such a node would revoke a success the client was told about, so every
// candidate must still match its snapshot and its whole ancestor chain must still lead to the
// COPY's root through the expected parents and names. Anything that changed is left alone; the
// enclosing directories are then kept too because they still have a live child.
//
// The rollback never looks the destination up by name. After a conflict with a concurrent MKCOL or
// COPY, the destination name belongs to another request's already-successful node.
//
// Best effort and loud when it fails: the alternative is a half-populated folder presented as a
// copy that failed. Trashing rather than purging keeps the rolled-back nodes recoverable.
func (h *Handler) rollbackCopy(ctx context.Context, a authz.Actor, created []createdNode,
	dstName string, r *http.Request) {

	if len(created) == 0 || copyWasOverwrite(r.Context()) {
		return
	}
	byID := make(map[uuid.UUID]createdNode, len(created))
	for _, c := range created {
		byID[c.ID] = c
	}
	root := created[0]

	var trashed []uuid.UUID
	err := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		// chainOK proves the node is still under this COPY's root at the exact parent and name
		// the COPY gave it, and that every ancestor between it and the root is unchanged too.
		// A moved subtree therefore fails for its top node AND for all of its descendants, so
		// reverse-order processing cannot delete the moved descendants one by one.
		chainCache := make(map[uuid.UUID]bool, len(created))
		var chainOK func(n store.Node) bool
		chainOK = func(n store.Node) bool {
			if ok, seen := chainCache[n.ID]; seen {
				return ok
			}
			exp, known := byID[n.ID]
			if !known {
				chainCache[n.ID] = false
				return false
			}
			good := n.Name == exp.Name && n.ParentID != nil && *n.ParentID == exp.ParentID
			if good && n.ID != root.ID {
				parent, perr := tx.Node(ctx, *n.ParentID)
				if perr != nil {
					good = false
				} else {
					good = chainOK(parent)
				}
			}
			chainCache[n.ID] = good
			return good
		}

		for i := len(created) - 1; i >= 0; i-- {
			exp := created[i]
			n, nerr := tx.Node(ctx, exp.ID)
			if errors.Is(nerr, store.ErrNotFound) {
				// Already gone (a concurrent DELETE, or a parent trashed it). Nothing to do.
				continue
			}
			if nerr != nil {
				return nerr
			}
			if !chainOK(n) {
				// The winner of a concurrent PUT/MOVE owns the current state; leave it.
				continue
			}
			// An intercepted dead-property write is a successful PROPPATCH that leaves the node's
			// ETag/size/mtime untouched. This is an interim guard for the reproduced case, not a
			// general generation: ACL grants, shares and other side tables still need the
			// operation-visibility/revision contract before the rollback can be called complete.
			// Adding one more field per table is not the fix; see
			// docs/reference/tree-storage-consistency.md.
			if props, derr := tx.DeadProps(ctx, exp.ID); derr != nil {
				return derr
			} else if len(props) > 0 {
				continue
			}
			// A successful LOCK changes no node field the snapshot compares, so it is invisible
			// to every check above — and the rollback would trash a file whose lock the client
			// was told had succeeded, then revoke the lock with it (R11). A lock is a success
			// the same way a PROPPATCH is, so it gets the same treatment: leave the node alone
			// and let the operator find it.
			if _, lerr := tx.ActiveLock(ctx, exp.ID); lerr == nil {
				continue
			} else if !errors.Is(lerr, store.ErrNotFound) {
				return lerr
			}
			if exp.IsDir {
				kids, kerr := tx.Children(ctx, exp.ID, 1)
				if kerr != nil {
					return kerr
				}
				if len(kids) > 0 {
					// A live child is a concurrent addition or a descendant this COPY did not
					// remove (its own children were processed and trashed first). Trashing the
					// directory would take another request's node with it.
					continue
				}
			} else if n.ETag != exp.ETag || n.Size != exp.Size || !n.ModifiedAt.Equal(exp.ModifiedAt) {
				// A successful PUT changed this file after the COPY created it.
				continue
			}
			if terr := tx.Trash(ctx, exp.ID); terr != nil {
				return terr
			}
			trashed = append(trashed, exp.ID)
		}
		if len(trashed) == 0 {
			return nil
		}
		// The rollback is an auditable event in its own right: a client that saw an error and an
		// operator looking at an item in the trash both need to know why it is there. The highest
		// node rolled back is the most useful target: the root when the whole copy was removed.
		highest := trashed[len(trashed)-1]
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "system", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.write", TargetKind: "node", TargetID: highest.String(),
			Result: "denied",
			Metadata: map[string]any{
				"name":     dstName,
				"rollback": "nodes created by the failed directory copy trashed",
				"nodes":    len(trashed),
			},
		})
	})
	if err != nil {
		h.logf("copy rollback: FAILED for destination %q: %v — the partial copy remains and "+
			"should be removed by hand", dstName, err)
		return
	}
	if len(trashed) == 0 {
		return
	}
	h.logf("copy rollback: trashed %d node(s) created by a failed directory copy at %q",
		len(trashed), dstName)
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
