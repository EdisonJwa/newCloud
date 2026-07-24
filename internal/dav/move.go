package dav

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

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

	if len(rejected) == 0 {
		_ = h.meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
			for _, p := range props {
				if err := tx.SetDeadProp(ctx, node.ID, p.Space, p.Name, p.Value); err != nil {
					return err
				}
			}
			return nil
		})
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<D:multistatus xmlns:D="DAV:">` + "\n")
	b.WriteString("<D:response><D:href>" + xmlEscape(r.URL.Path) + "</D:href>")

	if len(rejected) > 0 {
		b.WriteString("<D:propstat><D:prop>")
		for _, name := range rejected {
			b.WriteString("<D:" + name + "/>")
		}
		b.WriteString("</D:prop><D:status>HTTP/1.1 403 Forbidden</D:status></D:propstat>")
	} else {
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
	verb := authz.PermWrite
	if move {
		verb = authz.PermDelete
	}
	if !srcCap.Can(authz.PermRead) || !srcCap.Can(verb) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}

	destPath := r.Header.Get("Destination")
	if destPath == "" {
		writeXMLError(w, http.StatusBadRequest, "")
		return
	}
	// Destination is an absolute URL; only the path is meaningful here, and a
	// different host would be an SSRF vector.
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
		if err := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, existing.ID)
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
		if err == nil {
			_ = h.content.Move(ctx, layout.Node{Owner: a.ID().String(), Path: srcRel, IsDir: src.IsDir()},
				layout.Node{Owner: a.ID().String(), Path: dstRel, IsDir: src.IsDir()})
		}
	} else {
		err = h.copyNode(ctx, a, src, dstParent, dstName, dstRel, r)
	}

	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}

	w.WriteHeader(http.StatusCreated)
}

// copyNode copies a file or a directory subtree.
func (h *Handler) copyNode(ctx context.Context, a authz.Actor, src store.Node,
	dstParent store.Node, dstName, dstRel string, r *http.Request) error {

	if src.IsDir() {
		// Directories are copied by walking the source subtree. The limit is the
		// same PROPFIND cap, so a copy cannot be used to enumerate more than a
		// listing could.
		return h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			newDir, err := tx.CreateDir(ctx, store.NewNode{
				OwnerID: a.ID(), ParentID: dstParent.ID, Name: dstName,
			})
			if err != nil {
				return err
			}
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.write", TargetKind: "node", TargetID: newDir.ID.String(),
				Result: "ok", Metadata: map[string]any{"name": dstName, "kind": "dir", "copied_from": src.Name},
			})
		})
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
			BlobID: h.blobIDFor(ctx, res.Hash), BlobHash: res.Hash,
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
