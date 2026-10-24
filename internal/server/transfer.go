package server

// Copy, move and ZIP over the JSON API.
//
// The file manager could upload, rename and delete, and nothing else. COPY and MOVE existed only as
// WebDAV methods, so the two operations people use constantly had no browser path at all: a
// multi-file selection could only be deleted, and a single file could only be renamed in place. That
// DAV can copy does not make the product able to copy ??the distinction the 2026-10-23 review drew,
// and the reason this file exists.
//
// Three decisions worth stating up front:
//
//   - ONE batch endpoint rather than N requests from the client. A partial failure has to be
//     reportable: the caller needs to know which items moved and which did not, and a client-side
//     loop can only offer that by correlating independent responses. The endpoint answers per item,
//     in order, in the same code vocabulary as the single-item routes.
//
//   - Overwrites go through internal/swap, the same implementation the DAV handler uses. The first
//     version of this file had its own, simpler swap, and it reproduced the tree-layout failure DAV
//     had already been fixed for: on a path-backed layout the old destination still occupies its
//     path, so installing the replacement before moving it aside fails with ENOTEMPTY and leaves the
//     metadata naming bytes that were never installed. One correct sequence, one implementation.
//
//   - A subtree is resolved in ONE walk that can prove it saw everything (completeTree). The first
//     version recursed through Tx.Children, which truncates at the configured page size and says
//     nothing about it ??a folder one row over the limit copied "successfully" with a file missing,
//     and generated a valid ZIP with an entry omitted. A transfer either covers the whole tree or
//     fails; "most of it" is not a result anybody asked for.

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/dircommit"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/swap"
	"github.com/EdisonJwa/newCloud/internal/treewalk"
)

const (
	// batchMaxItems bounds one request. The UI never sends more than a screenful of rows; the bound
	// exists so a script cannot turn one request into an unbounded operation.
	batchMaxItems = 200
	// transferMaxDepth guards the ancestor walks in this file (insideSubtree, completeTree). It is a
	// safety valve against a cycle introduced by a bug, not a product limit.
	transferMaxDepth = 64
	// archiveMaxEntries bounds one archive. Over it, the request is refused BEFORE any byte is
	// written: a truncated archive is a valid ZIP that silently lost files, which is worse than an
	// error because every extraction tool reports success.
	archiveMaxEntries = 20_000
)

// statusError is a refusal that carries the status and code the API answers with.
//
// The batch endpoint answers per ITEM, so a refusal cannot simply write a response ??the remaining
// items still have to be attempted, and the caller has to be told which one failed and why. A typed
// error keeps the code the client branches on attached to the error while it travels.
type statusError struct {
	Status int
	Code   string
	Msg    string
}

func (e statusError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return e.Code
}

// refuse builds one of the typed refusals above.
func refuse(status int, code, msg string) error {
	return statusError{Status: status, Code: code, Msg: msg}
}

// writeTransferError maps an operation failure onto the API's single error shape.
func (s *Server) writeTransferError(w http.ResponseWriter, err error) {
	var se statusError
	if errors.As(err, &se) {
		writeError(w, se.Status, se.Code, se.Msg)
		return
	}
	s.writeStoreError(w, err)
}

// transferCode is the short code a batch result carries for a failed item.
//
// It uses the same vocabulary as writeStoreError, so a client can treat "the single-item route
// answered 409 conflict" and "the batch result says target_exists" as the same fact.
func transferCode(err error) string {
	var se statusError
	if errors.As(err, &se) {
		return se.Code
	}
	switch {
	case errors.Is(err, store.ErrConflict):
		return "target_exists"
	case errors.Is(err, store.ErrQuota):
		return "quota_exceeded"
	case errors.Is(err, store.ErrTrashFull):
		return "trash_full"
	case errors.Is(err, store.ErrCycle):
		return "cycle"
	case errors.Is(err, store.ErrAuditUnavailable):
		// The recorder is down, not the data ??the same distinction writeStoreError makes, kept
		// here because a batch item's code is what a client decides to retry on.
		return "audit_unavailable"
	case errors.Is(err, authz.ErrDenied), errors.Is(err, authz.ErrNotFound),
		errors.Is(err, store.ErrNotFound):
		return "not_found"
	}
	return "failed"
}

// batchResult is one item's outcome.
type batchResult struct {
	From  string `json:"from"`
	To    string `json:"to"`
	OK    bool   `json:"ok"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// batchItem is one source/destination pair.
type batchItem struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// handleBatch copies or moves a list of nodes.
//
// The response is a list rather than a single verdict because the interesting case is partial
// success: three of five files moved, and the two that did not failed for different reasons (one
// name already exists, one is not readable). Collapsing that into one status code would force the
// client to guess, and guessing here means telling somebody a file moved when it did not.
func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		Op        string      `json:"op"`
		Overwrite bool        `json:"overwrite"`
		Items     []batchItem `json:"items"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	op := strings.ToLower(strings.TrimSpace(body.Op))
	if op != "copy" && op != "move" {
		writeError(w, http.StatusBadRequest, "invalid_op", `op must be "copy" or "move"`)
		return
	}
	if len(body.Items) == 0 {
		writeError(w, http.StatusBadRequest, "empty_selection", "nothing was selected")
		return
	}
	if len(body.Items) > batchMaxItems {
		writeError(w, http.StatusBadRequest, "too_many_items",
			fmt.Sprintf("at most %d items can be transferred in one request", batchMaxItems))
		return
	}

	results := make([]batchResult, 0, len(body.Items))
	var moved, failed int

	for _, item := range body.Items {
		res := batchResult{From: item.From, To: item.To}
		if err := s.transferOne(r.Context(), r, actor, op, item.From, item.To, body.Overwrite); err != nil {
			res.Code = transferCode(err)
			res.Error = err.Error()
			failed++
		} else {
			res.OK = true
			moved++
		}
		results = append(results, res)
	}

	// A single item keeps the shape every other endpoint has: its own status and code, so one item
	// behaves exactly like the dedicated route rather than like a batch of one.
	if len(results) == 1 && failed == 1 {
		res := results[0]
		writeError(w, refusalStatus(res.Code), res.Code, res.Error)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"op":       op,
		"results":  results,
		"moved":    moved,
		"failed":   failed,
		"complete": failed == 0,
	})
}

// refusalStatus maps a transfer code back to the status it was raised with.
//
// Only needed for the single-item branch, where the batch has to answer like the dedicated route;
// the codes are a closed set, so the fallback is a defect rather than a case to design for.
func refusalStatus(code string) int {
	switch code {
	case "not_found":
		return http.StatusNotFound
	case "target_exists", "cycle":
		return http.StatusConflict
	case "locked":
		return http.StatusLocked
	case "quota_exceeded", "trash_full":
		return http.StatusInsufficientStorage
	case "audit_unavailable":
		return http.StatusServiceUnavailable
	case "too_large":
		return http.StatusRequestEntityTooLarge
	case "invalid_name", "invalid_target", "too_many_items", "empty_selection", "invalid_op":
		return http.StatusBadRequest
	case "content_move_failed":
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}

// transferOne performs one copy or one move.
func (s *Server) transferOne(
	ctx context.Context, r *http.Request, actor authz.Actor, op, from, to string, overwrite bool,
) error {
	if strings.Trim(from, "/") == "" || strings.Trim(to, "/") == "" {
		return refuse(http.StatusBadRequest, "invalid_target",
			"the root cannot be copied or moved, and both from and to are required")
	}

	if op == "move" {
		_, err := s.moveOne(ctx, r, actor, from, to, overwrite)
		return err
	}
	_, err := s.copyOne(ctx, r, actor, from, to, overwrite)
	return err
}

// layoutNodeFor builds the content-store address a namespace path maps to.
func layoutNodeFor(actor authz.Actor, rel string, isDir bool) layout.Node {
	return layout.Node{Owner: actor.ID().String(), Path: rel, IsDir: isDir}
}

/* ---------- copy ---------- */

// copyOne clones a node ??and, for a directory, its whole subtree ??into to.
func (s *Server) copyOne(
	ctx context.Context, r *http.Request, actor authz.Actor, from, to string, overwrite bool,
) (store.Node, error) {

	src, srcRel, err := s.transferSource(ctx, actor, from, false)
	if err != nil {
		return store.Node{}, err
	}

	parentRel, name := splitParent(to)
	if name == "" {
		return store.Node{}, refuse(http.StatusBadRequest, "invalid_target", "a target name is required")
	}
	parent, parentChain, err := s.writableParent(ctx, actor, parentRel, authz.PermWrite)
	if err != nil {
		return store.Node{}, err
	}
	// Everything below ??the destination path, and every path the swap derives from ParentRel ??is
	// built from the resolved parent, never from the caller's text. The file manager sends "/docs/x"
	// and a path-backed store refuses an absolute path.
	parentRel = relOfChain(parentChain)
	// Copying a directory into itself or into its own subtree would recurse forever: the walk would
	// keep finding the copy it is writing. Refused by identity, not by comparing paths, because a
	// path is not an identity.
	if src.IsDir() && s.insideSubtree(ctx, src.ID, parent.ID) {
		return store.Node{}, refuse(http.StatusConflict, "cycle", "cannot copy a folder into itself")
	}

	existing, err := s.prepareTarget(ctx, r, actor, parent, name, overwrite)
	if err != nil {
		return store.Node{}, err
	}

	// The subtree is resolved BEFORE anything is written, so a tree too large to copy completely is
	// refused instead of half-copied. This is the difference between an explicit failure and a
	// result that silently lost files.
	tree, err := s.completeTree(ctx, src, srcRel, traversalLimit())
	if err != nil {
		return store.Node{}, err
	}

	dstRel := path.Join(parentRel, name)

	if existing == nil {
		return s.copyResolved(ctx, r, actor, tree, parent.ID, name, dstRel)
	}

	return swap.Install(ctx, s.swapDeps(r), swap.Request{
		Owner: actor.ID().String(), Parent: parent, ParentRel: parentRel, DstName: name,
		Existing: *existing, StagedIsDir: src.IsDir(),
		// The stage is a duplicate this operation built and owns, so a failed install may remove it.
		Replacement: swap.Disposable(func(ctx context.Context, stagedName string) (store.Node, error) {
			return s.copyResolved(ctx, r, actor, tree, parent.ID, stagedName,
				path.Join(parentRel, stagedName))
		}),
		AuditRows: func(ctx context.Context, tx store.Tx, installed, aside store.Node) error {
			if aerr := tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.write", TargetKind: "node", TargetID: installed.ID.String(),
				Result: "ok", Metadata: map[string]any{
					"name": name, "copied_from": src.Name, "replaced": aside.Name,
					"replaced_node": aside.ID.String(),
				},
			}); aerr != nil {
				return aerr
			}
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.delete", TargetKind: "node", TargetID: aside.ID.String(),
				Result: "ok", Metadata: map[string]any{
					"name": name, "reason": "replaced by a COPY overwrite",
					"replaced_by": installed.ID.String(),
				},
			})
		},
		ActorID: actor.Ptr(), ActorEmail: actor.Email(),
		ActorIP: clientIP(r), UserAgent: r.UserAgent(),
	})
}

// transferTraversalLimitForTest lowers the completeness budget.
//
// Package-internal, like the DAV handler's rollback hooks, and for the same reason: the behaviour
// under test is "refuse rather than truncate", and reaching it honestly would mean building a
// ten-thousand-node tree. A guard that cannot be made to fail is not evidence.
var transferTraversalLimitForTest int

// SetTraversalLimitForTest lowers the completeness budget for one test, and returns a function that
// restores it.
//
// Exported for the same reason ThumbnailsAreIsolatedForTest is: the refusal path is otherwise
// unreachable without building a ten-thousand-node tree, and a guard that cannot be made to fail is
// not evidence. The alternative ??making every run build the tree ??would make the suite slow enough
// that somebody would delete the test.
func (s *Server) SetTraversalLimitForTest(n int) (restore func()) {
	prev := transferTraversalLimitForTest
	transferTraversalLimitForTest = n
	return func() { transferTraversalLimitForTest = prev }
}

// traversalLimit is the completeness budget for one subtree.
func traversalLimit() int {
	if transferTraversalLimitForTest > 0 {
		return transferTraversalLimitForTest
	}
	return subtreeCheckLimit
}

// transferSource resolves and authorizes the source half of a copy or a move.
//
// The two operations consume different rights, which is the same rule WebDAV applies ??stated in one
// place here so the browser path and the DAV path cannot disagree.
func (s *Server) transferSource(
	ctx context.Context, actor authz.Actor, from string, move bool,
) (store.Node, string, error) {

	nodes, err := s.deps.Meta.Resolve(ctx, actor.ID(), from)
	if err != nil {
		return store.Node{}, "", err
	}
	node := nodes[len(nodes)-1]
	// The content path is the resolved identity's, not the caller's string: the file manager sends
	// the listing's "/docs/a.txt" form and a path-backed layout refuses an absolute path. Everything
	// downstream (the source Open, the staged copy, the moved bytes) inherits this one answer.
	rel := relOfChain(nodes)

	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		return store.Node{}, "", err
	}

	if move {
		if !cap.Can(authz.PermDelete) || !cap.Can(authz.PermDownload) {
			return store.Node{}, "", refuse(http.StatusNotFound, "not_found", "no such file")
		}
		if node.IsDir() && !s.checkSubtreePermCtx(ctx, actor, node.ID, authz.PermDelete) {
			return store.Node{}, "", refuse(http.StatusNotFound, "not_found", "no such file")
		}
		return node, rel, nil
	}

	// COPY: read (to see it) plus download (the copy-taking primitive), and no write on the source.
	if !cap.Can(authz.PermRead) || !cap.Can(authz.PermDownload) {
		return store.Node{}, "", refuse(http.StatusNotFound, "not_found", "no such file")
	}
	if node.IsDir() && !s.checkSubtreePermCtx(ctx, actor, node.ID, authz.PermRead) {
		return store.Node{}, "", refuse(http.StatusNotFound, "not_found", "no such file")
	}
	return node, rel, nil
}

/* ---------- complete traversal ---------- */

// treeNode is a resolved subtree: a node and its children, already known to be complete.
//
// The walk itself lives in internal/treewalk so WebDAV COPY and this file cannot disagree about what
// "complete" means. That is not tidiness: the two surfaces had separate recursions over Tx.Children,
// and the review that fixed the batch path found WebDAV still truncating at the same page limit.
type treeNode = treewalk.Tree

// completeTree resolves a node's whole subtree, or fails.
//
// It exists because Tx.Children truncates at the configured page size and reports nothing about it:
// a folder with one row more than the limit was copied "successfully" with a file missing, and
// archived into a valid ZIP with an entry missing. Neither result looked wrong to the client, which
// is the worst property a data operation can have.
//
// treewalk.Resolve answers the completeness question directly (Tx.Descendants returns `complete`) and
// makes the checks this function used to make inline: a node the walk did not return, a chain at the
// store's depth bound, or a subtree over the budget is a refusal rather than a partial tree.
func (s *Server) completeTree(
	ctx context.Context, root store.Node, rootRel string, max int,
) (*treeNode, error) {

	tree, err := treewalk.Resolve(ctx, s.deps.Meta, root, rootRel, max)
	if err != nil {
		switch {
		case errors.Is(err, treewalk.ErrTooLarge):
			return nil, refuse(http.StatusRequestEntityTooLarge, "too_large",
				fmt.Sprintf("the folder has more than %d items; it cannot be transferred completely", max))
		case errors.Is(err, treewalk.ErrTooDeep):
			return nil, refuse(http.StatusRequestEntityTooLarge, "too_large",
				"the folder is nested deeper than a transfer can prove complete")
		case errors.Is(err, treewalk.ErrInconsistent):
			return nil, refuse(http.StatusConflict, "cycle", "the folder tree is inconsistent")
		default:
			return nil, err
		}
	}
	return tree, nil
}

// copyResolved writes one resolved subtree into dstParentID under dstName.
func (s *Server) copyResolved(
	ctx context.Context, r *http.Request, actor authz.Actor,
	n *treeNode, dstParentID uuid.UUID, dstName, dstRel string,
) (store.Node, error) {

	if n.Node.IsDir() {
		var created store.Node
		// Physical first, metadata second, both checked. The previous version created the node
		// and discarded the Mkdir error (NC-R6-001), so a batch COPY answered
		// complete:true/failed:0 for a directory the tree layout never created.
		err := dircommit.Create(ctx, s.deps.Content,
			layoutNodeFor(actor, dstRel, true),
			func(ctx context.Context) error {
				return s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
					dir, cerr := tx.CreateDir(ctx, store.NewNode{
						OwnerID: actor.ID(), ParentID: dstParentID, Name: dstName,
					})
					if cerr != nil {
						return cerr
					}
					created = dir
					return tx.Audit(ctx, store.AuditEntry{
						ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
						ActorIP: clientIP(r), UserAgent: r.UserAgent(),
						Action: "file.write", TargetKind: "node", TargetID: dir.ID.String(),
						Result: "ok",
						Metadata: map[string]any{
							"name": dstName, "kind": "dir", "copied_from": n.Rel,
						},
					})
				})
			})
		if err != nil {
			return store.Node{}, err
		}

		for _, child := range n.Children {
			if _, cerr := s.copyResolved(ctx, r, actor, child, created.ID, child.Node.Name,
				path.Join(dstRel, child.Node.Name)); cerr != nil {
				return store.Node{}, cerr
			}
		}
		return created, nil
	}

	rc, err := s.deps.Content.Open(ctx, layoutNodeFor(actor, n.Rel, false))
	if err != nil {
		return store.Node{}, err
	}
	defer rc.Close()

	res, err := s.deps.Content.Put(ctx, layoutNodeFor(actor, dstRel, false), rc, n.Node.Size)
	if err != nil {
		return store.Node{}, err
	}

	etag := etagFor(res.Hash)
	bytesCopied := res.Size
	var created store.Node
	err = s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		file, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: actor.ID(), ParentID: dstParentID, Name: dstName,
			Size: res.Size, Mime: n.Node.Mime, ETag: etag,
			BlobID: s.deps.Meta.BlobIDFor(ctx, res.Hash, res.Size), BlobHash: res.Hash,
		})
		if cerr != nil {
			return cerr
		}
		created = file
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.write", TargetKind: "node", TargetID: file.ID.String(),
			Result: "ok", Bytes: &bytesCopied,
			Metadata: map[string]any{"name": dstName, "copied_from": n.Rel},
		})
	})
	if err != nil {
		return store.Node{}, err
	}
	return created, nil
}

/* ---------- move ---------- */

// prepareTarget decides what to do about a name the destination already has.
//
// Refusing is the default, because the client has to be able to ask: "replace it?" is a question,
// and an overwrite nobody confirmed is a deletion. Replacing additionally needs the authority an
// explicit DELETE of that node would need, and the lock check a write would need.
func (s *Server) prepareTarget(
	ctx context.Context, r *http.Request, actor authz.Actor,
	parent store.Node, name string, overwrite bool,
) (*store.Node, error) {

	var existing *store.Node
	if err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.ChildByName(ctx, parent.ID, name)
		if cerr == nil {
			existing = &n
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}
	if !overwrite {
		return nil, refuse(http.StatusConflict, "target_exists",
			"something with that name already exists there")
	}

	cap, err := s.deps.Authz.Authorize(actor, existing.ID)
	if err != nil {
		return nil, err
	}
	if !cap.Can(authz.PermDelete) {
		return nil, refuse(http.StatusNotFound, "not_found", "no such file")
	}

	token := ""
	if r != nil {
		token = submittedLockToken(r)
	}
	blocked, err := s.lockBlocksWrite(ctx, actor, existing.ID, token)
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, refuse(http.StatusLocked, "locked",
			"the file being replaced is locked; submit its lock token")
	}
	return existing, nil
}

// swapDeps builds the shared swap dependencies for this request.
func (s *Server) swapDeps(r *http.Request) swap.Deps {
	trashCap := int64(0)
	if r != nil {
		trashCap = s.maxTrashBytes(r.Context())
	}
	return swap.Deps{
		Meta: s.deps.Meta, Content: s.deps.Content, Log: s.deps.Log, TrashCap: trashCap,
	}
}

// relocationAudit writes the audit row for a relocation, inside the transaction that performs it.
//
// A parameter rather than a fixed row because the same relocation serves two opposite meanings: the
// move a client asked for, and the compensating move that returns a borrowed source after a failed
// overwrite. The second must not be recorded as a success it did not achieve.
type relocationAudit func(ctx context.Context, tx store.Tx, moved store.Node) error

// relocate moves one node to (parentID, name) and puts its bytes at dstRel.
//
// The body lives in internal/swap, because WebDAV performs the same operation and its own copy of it
// is where the fourth review (2026-10-23) found a MOVE overwriting its own destination and a failed
// MOVE that had already trashed a folder. One implementation, one compensation, one place to fix.
func (s *Server) relocate(
	ctx context.Context, src store.Node, srcRel string, parentID uuid.UUID, name, dstRel string,
	audit relocationAudit,
) (store.Node, error) {

	moved, err := swap.Relocate(ctx,
		swap.Deps{Meta: s.deps.Meta, Content: s.deps.Content, Log: s.deps.Log},
		swap.Relocation{
			Node: src, FromRel: srcRel, ParentID: parentID, Name: name, ToRel: dstRel,
			Audit: audit,
		})
	if err != nil {
		if errors.Is(err, swap.ErrContentMove) {
			// swap.Relocate put the metadata back before returning, so "nothing was changed" is a
			// statement of fact rather than a hope.
			return store.Node{}, refuse(http.StatusInternalServerError, "content_move_failed",
				"the file could not be moved; nothing was changed")
		}
		return store.Node{}, err
	}
	return moved, nil
}

// moveAudit is the audit row a successful relocation writes.
func moveAudit(r *http.Request, actor authz.Actor, meta map[string]any) relocationAudit {
	return func(ctx context.Context, tx store.Tx, moved store.Node) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.move", TargetKind: "node", TargetID: moved.ID.String(),
			Result: "ok", Metadata: meta,
		})
	}
}

// unstageMove returns a moved source node to where it came from, after a failed overwrite.
//
// This is the compensation for a MOVE overwrite that did not commit, and it is the inverse of the
// staging step: the staged node IS the caller's own file, renamed and (on a path-backed layout)
// physically relocated. Deleting it ??the correct cleanup for a COPY's staged duplicate ??deletes
// the user's data. The 2026-10-23 review reproduced exactly that: a full trash made the overwrite
// answer 507 trash_full, and the source path then answered 404 because the rollback had removed its
// bytes and trashed its row.
//
// The node's identity (id, versions, shares, permissions) is untouched by a rename and a reparent,
// so relocating it back restores all of that along with the name and the bytes. A failure here is
// loud and non-destructive: the node stays at the intermediate name, intact, where an operator can
// find it ??never deleted.
func (s *Server) unstageMove(
	ctx context.Context, r *http.Request, actor authz.Actor,
	staged store.Node, original store.Node, originalRel, reason string,
) error {

	if original.ParentID == nil {
		return refuse(http.StatusInternalServerError, "content_move_failed",
			"the source has no parent to return to")
	}

	// The staged node's current path comes from its metadata, so this does not depend on the caller
	// remembering the reserved name it was given.
	stagedRel, err := s.pathOfNode(ctx, staged.OwnerID, staged.ID)
	if err != nil {
		return err
	}

	_, err = s.relocate(ctx, staged, stagedRel, *original.ParentID, original.Name, originalRel,
		func(ctx context.Context, tx store.Tx, moved store.Node) error {
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "system", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.move", TargetKind: "node", TargetID: moved.ID.String(),
				Result: "denied",
				Metadata: map[string]any{
					"name": original.Name, "rollback": reason,
				},
			})
		})
	return err
}

// moveOne relocates one node inside the caller's own namespace.
//
// Extracted from handleMove so the single-item route and the batch route share one implementation.
// A second copy of this logic is how the compensation in relocate goes missing from one of them ??// and a move whose metadata changed while its bytes did not looks like success to the client, which
// is the shape of data loss this code keeps trying to avoid.
func (s *Server) moveOne(
	ctx context.Context, r *http.Request, actor authz.Actor, from, to string, overwrite bool,
) (store.Node, error) {

	src, srcRel, err := s.transferSource(ctx, actor, from, true)
	if err != nil {
		return store.Node{}, err
	}

	// A lock on the source blocks moving it away from under the holder (§5.3).
	token := ""
	if r != nil {
		token = submittedLockToken(r)
	}
	blocked, err := s.lockBlocksWrite(ctx, actor, src.ID, token)
	if err != nil {
		return store.Node{}, err
	}
	if blocked {
		return store.Node{}, refuse(http.StatusLocked, "locked",
			"the file is locked; submit its lock token to write")
	}

	parentRel, name := splitParent(to)
	if name == "" {
		return store.Node{}, refuse(http.StatusBadRequest, "invalid_name", "a target name is required")
	}
	parent, parentChain, err := s.writableParent(ctx, actor, parentRel, authz.PermWrite)
	if err != nil {
		return store.Node{}, err
	}
	// As in copyOne: the destination path and everything the swap derives from ParentRel come from
	// the resolved parent, not from the listing's absolute spelling.
	parentRel = relOfChain(parentChain)
	if src.IsDir() && s.insideSubtree(ctx, src.ID, parent.ID) {
		return store.Node{}, refuse(http.StatusConflict, "cycle", "cannot move a folder into itself")
	}

	existing, err := s.prepareTarget(ctx, r, actor, parent, name, overwrite)
	if err != nil {
		return store.Node{}, err
	}

	// A move onto itself (same parent, same name) is a no-op, not an overwrite.
	if existing != nil && existing.ID == src.ID {
		return src, nil
	}

	dstRel := path.Join(parentRel, name)

	if existing == nil {
		return s.relocate(ctx, src, srcRel, parent.ID, name, dstRel,
			moveAudit(r, actor, map[string]any{"from": from, "to": to}))
	}

	// The replacement is the source itself, moved to a reserved sibling name first. That makes the
	// dangerous part ??freeing the destination path on a path-backed layout ??the same operation the
	// non-overwrite path performs.
	//
	// It is BORROWED, not disposable: there is no duplicate here, only the caller's own node at a
	// temporary name. If the install does not commit the only correct outcome is that the node goes
	// back where it came from; deleting it (the right cleanup for a COPY's stage) destroys the file.
	return swap.Install(ctx, s.swapDeps(r), swap.Request{
		Owner: actor.ID().String(), Parent: parent, ParentRel: parentRel, DstName: name,
		Existing: *existing, StagedIsDir: src.IsDir(),
		Replacement: swap.Borrowed(
			func(ctx context.Context, stagedName string) (store.Node, error) {
				return s.relocate(ctx, src, srcRel, parent.ID, stagedName,
					path.Join(parentRel, stagedName),
					moveAudit(r, actor, map[string]any{"from": from, "to": to, "staged": true}))
			},
			func(ctx context.Context, staged store.Node) error {
				return s.unstageMove(ctx, r, actor, staged, src, srcRel,
					"staged MOVE of an overwrite that did not commit")
			},
		),
		AuditRows: func(ctx context.Context, tx store.Tx, installed, aside store.Node) error {
			if aerr := tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.move", TargetKind: "node", TargetID: installed.ID.String(),
				Result: "ok", Metadata: map[string]any{
					"from": from, "to": to, "replaced": aside.Name,
					"replaced_node": aside.ID.String(),
				},
			}); aerr != nil {
				return aerr
			}
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.delete", TargetKind: "node", TargetID: aside.ID.String(),
				Result: "ok", Metadata: map[string]any{
					"name": name, "reason": "replaced by a MOVE",
					"replaced_by": installed.ID.String(),
				},
			})
		},
		ActorID: actor.Ptr(), ActorEmail: actor.Email(),
		ActorIP: clientIP(r), UserAgent: r.UserAgent(),
	})
}

// insideSubtree reports whether nodeID is rootID or lies below it.
func (s *Server) insideSubtree(ctx context.Context, rootID, nodeID uuid.UUID) bool {
	cur := nodeID
	for depth := 0; depth < transferMaxDepth; depth++ {
		if cur == rootID {
			return true
		}
		var n store.Node
		err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			got, gerr := tx.Node(ctx, cur)
			n = got
			return gerr
		})
		if err != nil || n.ParentID == nil {
			return false
		}
		cur = *n.ParentID
	}
	return false
}

/* ---------- zip ---------- */

// handleZip streams a ZIP of the selected nodes.
//
// Everything is authorized and RESOLVED before the first byte is written: the subtree of every
// selection is walked to completion, and a tree too large to include entirely is refused outright.
// Both halves matter. Once the archive headers are on the wire there is no way to answer 404, and a
// truncated archive is not an error a client can detect ??it is a valid ZIP that lost files, which
// every extraction tool reports as a success.
func (s *Server) handleZip(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	paths := r.URL.Query()["path"]
	if len(paths) == 0 {
		writeError(w, http.StatusBadRequest, "empty_selection", "name at least one path")
		return
	}
	if len(paths) > batchMaxItems {
		writeError(w, http.StatusBadRequest, "too_many_items",
			fmt.Sprintf("at most %d items can be archived in one request", batchMaxItems))
		return
	}

	trees := make([]*treeNode, 0, len(paths))
	for _, p := range paths {
		if strings.Trim(p, "/") == "" {
			writeError(w, http.StatusBadRequest, "invalid_target", "the root cannot be archived")
			return
		}
		nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), p)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		node := nodes[len(nodes)-1]

		cap, err := s.deps.Authz.Authorize(actor, node.ID)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		// Reading a copy is `download`, not merely `read`: an ACL can grant metadata visibility
		// without the right to take the bytes out.
		if !cap.Can(authz.PermRead) || !cap.Can(authz.PermDownload) {
			writeError(w, http.StatusNotFound, "not_found", "no such file or directory")
			return
		}
		if node.IsDir() && !s.checkSubtreePerm(r, actor, node.ID, authz.PermRead) {
			writeError(w, http.StatusNotFound, "not_found", "no such file or directory")
			return
		}

		// Bounded by BOTH budgets: the archive's own entry cap, and the completeness budget every
		// walk in this file obeys. Whichever is smaller binds, so an archive can never be assembled
		// from a walk that could not prove it saw everything.
		limit := archiveMaxEntries
		if t := traversalLimit(); t < limit {
			limit = t
		}
		tree, err := s.completeTree(r.Context(), node, relOfChain(nodes), limit)
		if err != nil {
			s.writeTransferError(w, err)
			return
		}
		trees = append(trees, tree)
	}

	// A budget check before the header, not during the stream.
	total := 0
	for _, t := range trees {
		total += treeEntries(t)
	}
	if total > archiveMaxEntries {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("the selection has more than %d items; narrow it down", archiveMaxEntries))
		return
	}

	names := make([]string, 0, len(trees))
	for _, t := range trees {
		names = append(names, t.Node.Name)
	}

	// Two selected items can share a name (they live in different folders). An archive cannot hold
	// two entries at one path without one silently replacing the other on extraction.
	used := map[string]int{}
	archiveNameFor := func(base string) string {
		if used[base] == 0 {
			used[base]++
			return base
		}
		used[base]++
		ext := path.Ext(base)
		return fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(base, ext), used[base], ext)
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(archiveFileName(names)))
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	w.Header().Set("Cache-Control", "no-store")

	zw := zip.NewWriter(w)
	written := 0
	for _, t := range trees {
		if err := s.zipTree(r.Context(), zw, actor, t, archiveNameFor(t.Node.Name)); err != nil {
			// The status line is already sent. The honest remaining move is to abort the response so
			// the client sees a truncated transfer rather than a complete-looking partial archive.
			// http.ErrAbortHandler is what net/http treats as "stop, say nothing more"; chi's
			// Recoverer re-panics it rather than turning it into a 500 halfway through a body.
			s.deps.Log.Error("zip: aborting a partial archive", "path", t.Rel, "error", err)
			panic(http.ErrAbortHandler)
		}
		written += treeEntries(t)
	}
	if err := zw.Close(); err != nil {
		s.deps.Log.Warn("zip: closing the archive failed", "error", err)
		return
	}

	// One audit row for the archive, not one per member: this is the event an owner would look for,
	// and a folder of a thousand files would otherwise bury the log in noise. A failure here is
	// logged rather than surfaced ??reads continue while the audit store is unavailable, which is the
	// documented policy (deploy/audit-failure-policy.md), and the bytes are already sent.
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.download", TargetKind: "node", Result: "ok",
			Metadata: map[string]any{"archive": true, "entries": written, "paths": paths},
		})
	}); err != nil {
		s.deps.Log.Warn("zip: the archive was sent but its audit row could not be written",
			"entries", written, "error", err)
	}
}

// treeEntries counts the nodes in a resolved subtree, including its root.
func treeEntries(n *treeNode) int {
	count := 1
	for _, c := range n.Children {
		count += treeEntries(c)
	}
	return count
}

// zipTree writes one resolved subtree into the archive.
func (s *Server) zipTree(
	ctx context.Context, zw *zip.Writer, actor authz.Actor, n *treeNode, name string,
) error {
	if n.Node.IsDir() {
		// An explicit entry, so an empty folder survives extraction. Without it a folder that happens
		// to contain no files simply does not appear in the archive.
		if _, err := zw.Create(name + "/"); err != nil {
			return err
		}
		for _, child := range n.Children {
			if err := s.zipTree(ctx, zw, actor, child, path.Join(name, child.Node.Name)); err != nil {
				return err
			}
		}
		return nil
	}

	rc, err := s.deps.Content.Open(ctx, layoutNodeFor(actor, n.Rel, false))
	if err != nil {
		return err
	}
	defer rc.Close()

	// A real modification time: a zero one makes every extracted file look like it was written in
	// 1979, which loses information the archive could have carried.
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
	if !n.Node.ModifiedAt.IsZero() {
		hdr.Modified = n.Node.ModifiedAt
	}
	f, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, rc)
	return err
}

// archiveFileName picks the download's filename from what was selected.
func archiveFileName(names []string) string {
	if len(names) == 1 {
		base := strings.TrimSuffix(names[0], path.Ext(names[0]))
		if base == "" {
			base = names[0]
		}
		return base + ".zip"
	}
	return "selection.zip"
}
