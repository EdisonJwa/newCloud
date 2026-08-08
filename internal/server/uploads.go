package server

// Resumable upload endpoints.
//
// The single-request upload stays: it is the right shape for the files people actually send
// through a browser, and adding chunks to it would make the common case slower to reason about.
// These endpoints exist for the case it cannot serve — a large file on a link that drops.
//
// The protocol is deliberately small:
//
//	POST   /api/files/uploads             -> {id, offset, chunk_size}
//	PUT    /api/files/uploads/{id}        -> Content-Range: bytes a-b/total
//	POST   /api/files/uploads/{id}/complete
//	DELETE /api/files/uploads/{id}
//
// It is not tus and not S3 multipart. Both are larger protocols with their own state machines;
// this covers sequential chunking with resume, which is the whole requirement, without taking
// on a specification the client would also have to implement correctly.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EdisonJwa/newCloud/internal/authz"
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
	if body.Size < 0 {
		writeError(w, http.StatusBadRequest, "invalid_size", "size must not be negative")
		return
	}
	// The cap is enforced before a byte is accepted, so a client cannot use sessions to write
	// past the file-size limit one chunk at a time.
	if body.Size > s.deps.Config.Limits.MaxFileBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the file exceeds the configured limit")
		return
	}

	if _, _, err := s.writableParent(r.Context(), actor, body.Path, authz.PermCreate); err != nil {
		s.writeStoreError(w, err)
		return
	}

	sess, err := s.deps.Uploads.Create(actor.ID(), body.Path, name, body.Size)
	if err != nil {
		if errors.Is(err, upload.ErrTooMany) {
			writeError(w, http.StatusTooManyRequests, "too_many_uploads", "too many unfinished uploads")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         sess.ID,
		"offset":     sess.Offset,
		"chunk_size": defaultChunkBytes,
	})
}

// handleUploadChunk appends one chunk.
//
// The response always carries the offset the server believes it is at, including on a conflict.
// That is what makes resume work: the client does not guess where to continue, it is told.
func (s *Server) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	id := chi.URLParam(r, "id")

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

	if size > s.deps.Config.Limits.MaxFileBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the chunk exceeds the configured limit")
		return
	}
	sess, err := s.deps.Uploads.Get(actor.ID(), id)
	if err != nil {
		s.writeUploadError(w, err)
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

	writeJSON(w, http.StatusOK, map[string]any{"offset": offset})
}

// handleUploadComplete publishes the staged bytes.
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

	parent, _, err := s.writableParent(r.Context(), actor, sess.Dir, authz.PermCreate)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	rel := path.Join(sess.Dir, sess.Name)
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
		// 409 with the true offset: the client resumes from here instead of restarting.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "offset_mismatch",
			"message": "resume from the returned offset",
			"offset":  conflict.Offset,
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
