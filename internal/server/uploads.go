package server

// Resumable upload endpoints.
//
// The single-request upload stays: it is the right shape for the files people actually send
// through a browser, and adding chunks to it would make the common case slower to reason about.
// These endpoints exist for the case it cannot serve — a large file on a link that drops.
//
// The protocol is deliberately small:
//
//	POST   /api/files/uploads             -> {id, offset, chunk_size, ranges}
//	GET    /api/files/uploads/{id}        -> {id, offset, chunk_size, ranges}
//	PUT    /api/files/uploads/{id}        -> Content-Range: bytes a-b/total
//	POST   /api/files/uploads/{id}/complete
//	DELETE /api/files/uploads/{id}
//
// It is not tus and not S3 multipart. Both are larger protocols with their own state machines;
// this covers parallel, out-of-order chunking with resume, which is the whole requirement, without
// taking on a specification the client would also have to implement correctly.
//
// `offset` is always the contiguous prefix [0, offset): a sequential client can ignore `ranges`
// and resume there. `ranges` is the full set of acknowledged half-open intervals, which is what a
// client sending several chunks at once needs in order to know which holes are left.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/upload"
)

const (
	// defaultChunkBytes is what the server suggests to the client. 8 MiB keeps a request short
	// enough to finish on a poor link and large enough that the per-request overhead disappears.
	defaultChunkBytes = 8 << 20
)

// handleUploadCreate starts a session.
func (s *Server) handleUploadCreate(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		Path string `json:"path"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	name := path.Base(body.Name)
	if body.Name == "" || name == "" || name == "." || name == "/" {
		writeError(w, http.StatusBadRequest, "invalid_name", "the file needs a name")
		return
	}
	// Resumable sessions stage on local disk and publish through the local content store;
	// a mount destination would stage locally only to stream remotely at complete, which
	// is the single-request upload's job. Refused here, at session start, rather than at
	// complete with gigabytes staged (ADR-0014).
	if _, external := s.externalPath(r, body.Path); external {
		writeError(w, http.StatusBadRequest, "external_not_supported",
			"resumable uploads target local storage; a single-request upload can write into a mount")
		return
	}
	// The store's own segment validator, applied before a session exists. The completion step
	// validates the staged name the same way; catching it here means a session is never created
	// for a name the store would refuse at the end, after the client has already uploaded every
	// chunk. ".." passes the three-way check above, which is exactly why the validator is asked.
	if verr := layout.ValidateSegment(name); verr != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "the file needs a usable name")
		return
	}
	if body.Size < 0 {
		writeError(w, http.StatusBadRequest, "invalid_size", "size must not be negative")
		return
	}
	// The cap is enforced before a byte is accepted, so a client cannot use sessions to write
	// past the file-size limit one chunk at a time.
	if body.Size > s.maxFileBytes(r.Context()) {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the file exceeds the configured limit")
		return
	}

	if _, chain, err := s.writableParent(r.Context(), actor, body.Path, authz.PermCreate); err != nil {
		s.writeStoreError(w, err)
		return
	} else {
		// The session carries the directory as a CONTENT path. The caller sends the listing's
		// "/docs", which a path-backed store refuses; canonicalising it once here means the chunks'
		// eventual Put (and the finish step, hours later) cannot disagree about the spelling.
		body.Path = relOfChain(chain)
	}

	// The staging cap is applied here, from the effective setting, rather than baked into the manager
	// at startup: an administrator lowering it must not have to restart the process for it to bite.
	// Two concurrent requests set the same value, so the write is idempotent; a change racing a request
	// can only make that request use the previous value, which is the same lag a cached read would give.
	s.deps.Uploads.SetMaxPendingBytes(s.maxPendingUploadBytes(r.Context()))
	sess, err := s.deps.Uploads.Create(actor.ID(), body.Path, name, body.Size)
	if err != nil {
		if errors.Is(err, upload.ErrTooMany) {
			writeError(w, http.StatusTooManyRequests, "too_many_uploads", "too many unfinished uploads")
			return
		}
		if errors.Is(err, upload.ErrPendingFull) {
			// 507 rather than 429: the count limit is a rate problem, this one is a space problem,
			// and "retry later" would be wrong advice — the client has to finish or abort something.
			writeError(w, http.StatusInsufficientStorage, "pending_uploads_full",
				"unfinished uploads are using the account's staging allowance")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	offset, ranges := sess.Progress()
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         sess.ID,
		"offset":     offset,
		"chunk_size": s.suggestedChunkBytes(r.Context()),
		"ranges":     ranges,
	})
}

// suggestedChunkBytes is the size advertised to a client. It is the smaller of the built-in
// default and the enforced per-request cap: advertising 8 MiB under a 1 MiB cap would make the
// client's first chunk fail with 413 and point at a setting the client never saw.
func (s *Server) suggestedChunkBytes(ctx context.Context) int64 {
	limit := s.chunkBytes(ctx)
	if limit <= 0 || limit > defaultChunkBytes {
		return defaultChunkBytes
	}
	return limit
}

// handleUploadStatus reports the ranges the server has acknowledged.
//
// Resume used to trust the last offset the browser recorded in localStorage. With out-of-order
// chunks that is not enough: the highest accepted byte can be far past the contiguous prefix, and
// the missing set can be in the middle. This endpoint is the authoritative answer, and it is
// owner-scoped exactly like the chunk endpoint.
func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	sess, err := s.deps.Uploads.Get(actor.ID(), chi.URLParam(r, "id"))
	if err != nil {
		s.writeUploadError(w, err)
		return
	}
	offset, ranges := sess.Progress()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         sess.ID,
		"offset":     offset,
		"chunk_size": s.suggestedChunkBytes(r.Context()),
		"ranges":     ranges,
	})
}

// handleUploadChunk appends one chunk.
//
// The response always carries the offset the server believes it is at, including on a conflict.
// That is what makes resume work: the client does not guess where to continue, it is told.
func (s *Server) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	id := chi.URLParam(r, "id")

	// The session lookup comes FIRST, before the request is validated at all.
	//
	// It used to sit after the Content-Range checks, which meant a request with a malformed range
	// got 400 without the ownership of the session ever being consulted — a caller could tell a
	// real endpoint from a fake one by which error came back. The response did not leak content,
	// so this was never a vulnerability, but "authenticate, then parse" is the rule that keeps it
	// from becoming one the next time a check is added above the lookup.
	//
	// Unknown, expired, finished and foreign sessions all produce the same 404 here, so this is
	// also the check that makes the route indistinguishable for a session belonging to somebody
	// else.
	sess, err := s.deps.Uploads.Get(actor.ID(), id)
	if err != nil {
		s.writeUploadError(w, err)
		return
	}

	start, end, total, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_range", err.Error())
		return
	}
	if end-start+1 <= 0 {
		writeError(w, http.StatusBadRequest, "bad_range", "the range is empty")
		return
	}
	size := end - start + 1

	if size > s.maxFileBytes(r.Context()) {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the chunk exceeds the configured limit")
		return
	}
	// limits.upload_chunk_bytes used to be validated and then never read, so a client could push
	// the whole file through one chunk and make the setting meaningless. A chunk limit that is not
	// enforced is not a limit: it does not bound the memory a single request can make the server
	// stage, which is the reason to have one.
	if limit := s.chunkBytes(r.Context()); limit > 0 && size > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "chunk_too_large",
			fmt.Sprintf("the chunk exceeds limits.upload_chunk_bytes (%d); split the upload into smaller ranges", limit))
		return
	}
	if total != sess.Total {
		writeError(w, http.StatusBadRequest, "bad_range", "the total does not match the declared size")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, size+1)
	offset, err := s.deps.Uploads.Append(r.Context(), actor.ID(), id, start, r.Body, size)
	if err != nil {
		s.writeUploadError(w, err)
		return
	}
	// Append may have acknowledged this range out of order, so `offset` alone is not enough for a
	// parallel client: the ranges show the holes that are still open.
	_, ranges := sess.Progress()
	writeJSON(w, http.StatusOK, map[string]any{"offset": offset, "ranges": ranges})
}

// handleUploadComplete publishes the staged bytes.
// handleUploadComplete publishes staged bytes.
//
// INV-03: the file becomes visible only after its content is persisted and its hash verified and the
// metadata row commits — a fault during any of those leaves no half-made file in a listing.
//
// Completion is a WRITE like any other, so it is arbitrated like any other. The first version
// checked only the parent's create permission and then published, which made the resumable path
// the one write in the product that answered 201 over a WebDAV lock, replaced a file the caller
// had no `overwrite` grant for, and honoured no precondition — every check the single-request
// upload makes, skipped by the one path a client can retry for hours. The guards below are the
// single-request handler's own sequence (files.go), kept in the same order so the two ways of
// writing the same bytes cannot drift apart again.
func (s *Server) handleUploadComplete(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	id := chi.URLParam(r, "id")

	file, sess, err := s.deps.Uploads.Finish(actor.ID(), id)
	if err != nil {
		s.writeUploadError(w, err)
		return
	}
	cleanup := func() {
		_ = file.Close()
		// The staged bytes are the server's own temporary file; removing it after publishing
		// is what keeps a burst of large uploads from filling the disk.
		_ = os.Remove(sess.StagingPath(s.uploadStagingDir()))
	}
	defer cleanup()

	parent, chain, err := s.writableParent(r.Context(), actor, sess.Dir, authz.PermCreate)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	rel := path.Join(relOfChain(chain), sess.Name)

	// The folded sibling at the target decides three things below: whether the completion is a
	// conflict (a name differing only in case or Unicode form — T-12), which lock rule applies,
	// and which quota delta applies.
	sibling, xerr := s.childAtTarget(r.Context(), parent.ID, sess.Name)
	if xerr != nil {
		s.writeStoreError(w, xerr)
		return
	}
	if sibling != nil && sibling.Name != sess.Name {
		writeError(w, http.StatusConflict, "conflict",
			"something whose name differs only in case or Unicode form already exists there")
		return
	}

	if sibling == nil || sibling.IsDir() {
		// Creating a new name is a membership change on the parent, so a Depth: 0 parent lock
		// blocks it (RFC 4918 §7.4) — the same rule the single-request upload applies. A name
		// that already belongs to a directory is the same question: the completion cannot
		// replace it, so what happens next is a store-level refusal, not a content overwrite.
		if !s.guardParentUnlocked(w, r, actor, parent.ID) {
			return
		}
	}
	if sibling != nil && !sibling.IsDir() {
		// Replacing an existing file needs `overwrite` (§4.2), and a WebDAV lock on the target
		// blocks the replacement (§5.3), as does a Depth: infinity lock on an ancestor — the
		// checks are the single-request upload's, in its order.
		if !s.guardUnlocked(w, r, actor, sibling.ID) {
			return
		}
		if !s.guardAncestorContentUnlocked(w, r, actor, sibling.ID) {
			return
		}
		parentCap, cerr := s.deps.Authz.Authorize(actor, parent.ID)
		if cerr != nil {
			s.writeStoreError(w, cerr)
			return
		}
		if !parentCap.Can(authz.PermOverwrite) {
			writeError(w, http.StatusNotFound, "not_found", "no such directory")
			return
		}
	}

	// Preconditions and quota are decided BEFORE the Put, for the same reason the single-request
	// upload decides them there: the tree layout publishes at the live path, so a completion that
	// was going to be refused must not replace the bytes it was going to be refused for. Both are
	// fast paths — publishFile re-runs the precondition inside WriteFile and CreateFile holds the
	// authoritative reservation — so a writer committing in between still fails safely.
	if !s.preconditionHoldsNow(r, sibling) {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed",
			"the file changed since you last read it")
		return
	}
	if !s.quotaHasRoomFor(r.Context(), actor.ID(), sess.Total, sibling) {
		writeError(w, http.StatusInsufficientStorage, "quota_exceeded", "your storage quota is full")
		return
	}

	res, err := s.deps.Content.Put(r.Context(), nodeFor(actor, rel), file, sess.Total)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	mime := guessType(sess.Name)
	// The precondition is re-evaluated here, at commit, not when the session was
	// created: an upload can take hours, and the target may have been replaced
	// meanwhile. This is what §6.1's "re-validate at commit" means in practice.
	node, err := s.publishFile(r.Context(), r, actor, parent.ID, sess.Name, rel, mime, res,
		preconditionFromHeader(r))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, toFileJSON(node, rel))
}

// handleUploadAbort discards a session.
func (s *Server) handleUploadAbort(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	if err := s.deps.Uploads.Abort(actor.ID(), chi.URLParam(r, "id")); err != nil {
		s.writeUploadError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeUploadError maps the package's errors onto the status codes the client branches on.
//
// Every upload failure goes through here, which is why the metric is recorded here rather than at
// each caller: one place to instrument, and no caller can forget. A 409 is counted separately from
// a real failure — resuming is how chunked uploads are supposed to work, and folding it into the
// failure rate would make a healthy client look broken.
func (s *Server) writeUploadError(w http.ResponseWriter, err error) {
	var conflict *upload.ConflictError
	if s.deps.Metrics != nil {
		switch {
		case errors.As(err, &conflict):
			s.deps.Metrics.ObserveUpload("conflict")
		case errors.Is(err, upload.ErrNotFound):
			s.deps.Metrics.ObserveUpload("session_gone")
		case errors.Is(err, upload.ErrTooMany):
			s.deps.Metrics.ObserveUpload("too_many")
		default:
			s.deps.Metrics.ObserveUpload("failed")
		}
	}

	switch {
	case errors.As(err, &conflict):
		// 409 with the true progress: the client sends what the ranges say is missing instead of
		// restarting. `offset_mismatch` is kept as the machine code because clients already branch
		// on it; the message distinguishes an overlapping chunk from an incomplete Finish, and the
		// ranges carry the information either way.
		message := "the chunk overlaps bytes the server already has; send only the returned missing ranges"
		if conflict.Incomplete {
			message = "the upload still has missing ranges"
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "offset_mismatch",
			"message": message,
			"offset":  conflict.Offset,
			"ranges":  conflict.Ranges,
		})
	case errors.Is(err, upload.ErrNotFound):
		// Same answer for unknown, expired, finished and foreign sessions.
		writeError(w, http.StatusNotFound, "not_found", "no such upload")
	case errors.Is(err, upload.ErrTooMany):
		writeError(w, http.StatusTooManyRequests, "too_many_uploads", "too many unfinished uploads")
	default:
		writeError(w, http.StatusBadRequest, "upload_failed", err.Error())
	}
}

// parseContentRange reads "bytes start-end/total".
//
// Strict on purpose: a lenient parser here would accept a range that does not describe the body
// and write bytes at an offset nobody asked for.
func parseContentRange(header string) (start, end, total int64, err error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, 0, 0, errors.New("Content-Range is required")
	}
	rest, ok := strings.CutPrefix(header, "bytes ")
	if !ok {
		return 0, 0, 0, errors.New(`Content-Range must look like "bytes 0-1023/2048"`)
	}
	span, totalPart, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, 0, 0, errors.New(`Content-Range is missing the total size`)
	}
	fromPart, toPart, ok := strings.Cut(span, "-")
	if !ok {
		return 0, 0, 0, errors.New(`Content-Range is missing the end offset`)
	}
	if start, err = strconv.ParseInt(fromPart, 10, 64); err != nil || start < 0 {
		return 0, 0, 0, errors.New("Content-Range has an invalid start")
	}
	if end, err = strconv.ParseInt(toPart, 10, 64); err != nil || end < start {
		return 0, 0, 0, errors.New("Content-Range has an invalid end")
	}
	if totalPart != "*" {
		if total, err = strconv.ParseInt(totalPart, 10, 64); err != nil || total <= 0 {
			return 0, 0, 0, errors.New("Content-Range has an invalid total")
		}
		if end >= total {
			return 0, 0, 0, fmt.Errorf("Content-Range ends at %d but the total is %d", end, total)
		}
	}
	return start, end, total, nil
}
