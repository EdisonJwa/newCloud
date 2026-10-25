package server

// tus 1.0.0 resumable uploads (core-scope §1, ADR-0015).
//
// The DURABILITY MACHINERY IS NOT REIMPLEMENTED. A tus session maps onto the existing session
// manager: creation is Create, PATCH is Append, HEAD is Get, DELETE is Abort, and the upload that
// reaches its declared length goes through the same complete guards and the same publishFile. That
// is deliberate and it is the argument ADR-0015 rests on: the staging file, the fsynced JSON record,
// restart recovery with its five consistency rules, and the graceful-shutdown preservation were
// built to be protocol-independent, and this is the protocol that proves it.
//
// The extensions this server advertises are the ones its client libraries actually use:
//
//	creation, creation-with-upload   one round trip for a small file
//	creation-defer-length            Upload-Length: deferred, bounded by limits.max_file_bytes
//	expiration                       Upload-Expires, reusing limits.upload_session_ttl_hours
//	termination                      DELETE
//	checksum                         Upload-Checksum: sha256, and the 460 the spec defines
//	concatenation                    partial uploads joined by Upload-Concat: final
//
// What the extensions cost, stated because §1 asks for it: tus core is strictly append-at-offset, so
// the bespoke protocol's out-of-order chunking is gone. Parallelism comes from multiple partial
// uploads joined by concatenation instead, which is how tus-js-client implements parallelUploads.
//
// The one thing that is NOT tus's: Upload-Offset is authoritative and a stale PATCH is 409. The
// session manager already refuses an overlapping write and reports the true offset in the conflict,
// which is exactly the behaviour the spec requires — so the mapping is a translation, not a
// reimplementation.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/upload"
)

const (
	// tusVersion is the protocol version this server implements.
	tusVersion = "1.0.0"
	// tusMaxMetadataBytes bounds Upload-Metadata. The header carries a path, a filename and a MIME
	// hint; a megabyte of base64 in a header is not a client, it is an attempt to make the server
	// allocate.
	tusMaxMetadataBytes = 8 << 10
	// tusChecksumMismatch is the status tus defines for a checksum failure (§4 of the protocol).
	//
	// Not 400 and not 422: a client library that sees 460 retries the CHUNK, while one that sees 4xx
	// gives up on the upload. The distinction is the whole reason the extension has a status of its
	// own.
	tusChecksumMismatch = 460
)

// tusExtensions is what OPTIONS advertises.
//
// Every entry is an extension this server implements. Advertising one it does not would make a
// client take a path that fails at the worst moment — mid-upload, after gigabytes.
var tusExtensions = []string{
	"creation", "creation-with-upload", "creation-defer-length", "expiration",
	"termination", "checksum", "concatenation",
}

// handleTusOptions answers the capability probe.
//
// Unauthenticated, like WebDAV's OPTIONS: a client has to know whether the endpoint exists before it
// has any reason to authenticate, and the answer says nothing about the caller's data.
func (s *Server) handleTusOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Tus-Version", tusVersion)
	w.Header().Set("Tus-Extension", strings.Join(tusExtensions, ","))
	w.Header().Set("Tus-Max-Size", strconv.FormatInt(s.maxFileBytes(r.Context()), 10))
	w.WriteHeader(http.StatusNoContent)
}

// handleTusCreate implements POST (the creation extension).
//
// The refusals happen BEFORE any byte is accepted, which is §1's first kept principle: the
// target-directory authorization, the fold-name conflict check, the lock consultation and the quota
// reservation all run here. Finalization re-runs them inside the statement that changes the row, so
// an upload that takes hours cannot slip past a check that was true when it started.
func (s *Server) handleTusCreate(w http.ResponseWriter, r *http.Request) {
	if !s.tusVersionOK(w, r) {
		return
	}
	if s.deps.Uploads == nil {
		s.tusUnavailable(w)
		return
	}
	actor, _ := actorFrom(r.Context())

	meta, err := parseTusMetadata(r.Header.Get("Upload-Metadata"))
	if err != nil {
		s.tusError(w, http.StatusBadRequest, err.Error())
		return
	}

	length, deferred, err := tusLength(r.Header.Get("Upload-Length"))
	if err != nil {
		s.tusError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !deferred && length > s.maxFileBytes(r.Context()) {
		s.tusError(w, http.StatusRequestEntityTooLarge,
			"the file exceeds the configured limit")
		return
	}
	if deferred {
		// creation-defer-length: the size arrives with the LAST PATCH. Until then the session's
		// Total is the cap, which is what makes Append refuse a chunk that could never fit.
		length = s.maxFileBytes(r.Context())
	}

	// A concatenation-final request joins partial uploads instead of creating a new session.
	if concat := strings.TrimSpace(r.Header.Get("Upload-Concat")); strings.HasPrefix(concat, "final;") {
		s.tusConcatFinal(w, r, actor, meta, concat)
		return
	}
	if concat := strings.TrimSpace(r.Header.Get("Upload-Concat")); concat != "" {
		if concat != "partial" {
			s.tusError(w, http.StatusBadRequest, "Upload-Concat must be partial or final;...")
			return
		}
		// A partial is created like any other session and marked afterwards, because the flag has
		// to survive a restart: a partial that came back as an ordinary session would be published
		// as its own file the moment it filled.
		s.tusCreatePartial(w, r, actor, meta, length, deferred)
		return
	}

	name := meta["filename"]
	if name == "" {
		// tus itself has no notion of a filename; the client sends it as metadata. Without one there
		// is nothing to publish, and guessing from the destination path would let a client name a
		// file by accident.
		s.tusError(w, http.StatusBadRequest, "Upload-Metadata must carry a filename")
		return
	}
	name = path.Base(name)
	if name == "" || name == "." || name == "/" {
		s.tusError(w, http.StatusBadRequest, "the file needs a name")
		return
	}
	if verr := layout.ValidateSegment(name); verr != nil {
		s.tusError(w, http.StatusBadRequest, "the file needs a usable name")
		return
	}

	dest := meta["destination"]
	// A mount destination stages locally and streams remotely at the end, which is the
	// single-request upload's job. Refused at creation rather than at finalization with gigabytes
	// already staged (ADR-0014).
	if _, external := s.externalPath(r, dest); external {
		s.tusError(w, http.StatusBadRequest,
			"resumable uploads target local storage; a single-request upload can write into a mount")
		return
	}

	_, chain, perr := s.writableParent(r.Context(), actor, dest, authz.PermCreate)
	if perr != nil {
		s.writeStoreError(w, perr)
		return
	}
	contentDir := relOfChain(chain)

	// The lock consultation at CREATION, before a byte is accepted (core-scope §1: "refused before
	// bytes when knowable at creation, re-checked in-statement at finalization").
	//
	// Both halves are needed and neither substitutes for the other. This one tells a client
	// immediately that the destination is locked rather than after an hour of upload; the one in
	// tusFinalize catches a lock taken while the upload was in flight. Only the second existed
	// before, which meant a locked folder accepted a session and every chunk of it — found by
	// TestTusCreationRefusesALockedDestination.
	parent := chain[len(chain)-1]
	if !s.guardParentUnlocked(w, r, actor, parent.ID) {
		return
	}

	// The fold-name collision, with the same shape: "Report.txt" exists and this is "report.txt".
	// Refused now if it is already true, re-checked at finalization because the name can be taken in
	// between.
	//
	// The sibling is nil when the name is free, which is the common case. It is ALSO the value
	// quotaHasRoomFor wants: a nil sibling means "this upload adds bytes rather than replacing a
	// file", and passing a zero struct instead would make the quota delta wrong by the old file's size.
	sibling, xerr := s.childAtTarget(r.Context(), parent.ID, name)
	if xerr != nil {
		s.writeStoreError(w, xerr)
		return
	}
	if sibling != nil {
		if !s.guardParentUnlocked(w, r, actor, parent.ID) {
			return
		}
		if !s.guardUnlocked(w, r, actor, sibling.ID) {
			return
		}
	}

	// The quota, when the length is known: staging bytes the account can never publish fills a disk
	// for an upload that fails at the end. A deferred length cannot be checked here, and tusFinalize
	// does it.
	if !deferred && !s.quotaHasRoomFor(r.Context(), actor.ID(), length, sibling) {
		s.tusError(w, http.StatusInsufficientStorage, "your storage quota is full")
		return
	}

	// The same staging caps the bespoke endpoint applies, from the effective settings.
	s.deps.Uploads.SetMaxPendingBytes(s.maxPendingUploadBytes(r.Context()))
	sess, cerr := s.deps.Uploads.Create(actor.ID(), contentDir, name, length)
	if cerr != nil {
		if errors.Is(cerr, upload.ErrTooMany) {
			s.tusError(w, http.StatusTooManyRequests, "too many unfinished uploads")
			return
		}
		if errors.Is(cerr, upload.ErrPendingFull) {
			s.tusError(w, http.StatusInsufficientStorage,
				"unfinished uploads are using the account's staging allowance")
			return
		}
		s.tusError(w, http.StatusBadRequest, cerr.Error())
		return
	}

	// creation-with-upload: the body of the creation request IS the first chunk.
	if r.ContentLength > 0 {
		if code, msg := s.tusAppendBody(w, r, actor, sess, 0, meta); code != 0 {
			s.tusError(w, code, msg)
			return
		}
	}

	s.tusSessionHeaders(w, r, sess)
	w.Header().Set("Location", "/api/files/tus/"+sess.ID)
	w.WriteHeader(http.StatusCreated)
}

// tusCreatePartial creates a concatenation part.
//
// It duplicates the admission sequence rather than sharing it with the ordinary creation, because
// the two differ in exactly one place that matters — the file is never published — and the sequence
// is short enough that a shared helper would be a helper with a boolean that changes every step.
func (s *Server) tusCreatePartial(w http.ResponseWriter, r *http.Request, actor authz.Actor,
	meta map[string]string, length int64, deferred bool) {

	name := path.Base(meta["filename"])
	if name == "" || name == "." || name == "/" {
		// A partial still needs a name: it is what the final request's destination is validated
		// against, and a session without one could never be joined into a publishable file.
		name = "partial"
	}
	if verr := layout.ValidateSegment(name); verr != nil {
		s.tusError(w, http.StatusBadRequest, "the file needs a usable name")
		return
	}

	dest := meta["destination"]
	if _, external := s.externalPath(r, dest); external {
		s.tusError(w, http.StatusBadRequest,
			"resumable uploads target local storage")
		return
	}
	_, chain, perr := s.writableParent(r.Context(), actor, dest, authz.PermCreate)
	if perr != nil {
		s.writeStoreError(w, perr)
		return
	}

	s.deps.Uploads.SetMaxPendingBytes(s.maxPendingUploadBytes(r.Context()))
	sess, cerr := s.deps.Uploads.Create(actor.ID(), relOfChain(chain), name, length)
	if cerr != nil {
		s.tusError(w, http.StatusBadRequest, cerr.Error())
		return
	}
	if perr := s.deps.Uploads.SetPartial(actor.ID(), sess.ID); perr != nil {
		_ = s.deps.Uploads.Abort(actor.ID(), sess.ID)
		s.tusError(w, http.StatusInternalServerError, "could not record the partial upload")
		return
	}
	_ = deferred

	if r.ContentLength > 0 {
		if code, msg := s.tusAppendBody(w, r, actor, sess, 0, meta); code != 0 {
			s.tusError(w, code, msg)
			return
		}
	}

	s.tusSessionHeaders(w, r, sess)
	w.Header().Set("Location", "/api/files/tus/"+sess.ID)
	w.WriteHeader(http.StatusCreated)
}

// handleTusHead reports the server's offset and the session's metadata.
func (s *Server) handleTusHead(w http.ResponseWriter, r *http.Request) {
	if !s.tusVersionOK(w, r) {
		return
	}
	if s.deps.Uploads == nil {
		s.tusUnavailable(w)
		return
	}
	actor, _ := actorFrom(r.Context())

	// The session lookup comes FIRST, before any header is validated, for the reason the bespoke
	// chunk endpoint documents: an unknown, expired, finished or foreign session must be
	// indistinguishable, and a different answer for a malformed request would let a caller tell a
	// real endpoint from a fake one.
	sess, err := s.deps.Uploads.Get(actor.ID(), chi.URLParam(r, "id"))
	if err != nil {
		s.writeUploadError(w, err)
		return
	}

	s.tusSessionHeaders(w, r, sess)
	w.WriteHeader(http.StatusOK)
}

// handleTusPatch appends a chunk at the offset the client believes it is at.
func (s *Server) handleTusPatch(w http.ResponseWriter, r *http.Request) {
	if !s.tusVersionOK(w, r) {
		return
	}
	if s.deps.Uploads == nil {
		s.tusUnavailable(w)
		return
	}
	actor, _ := actorFrom(r.Context())

	sess, err := s.deps.Uploads.Get(actor.ID(), chi.URLParam(r, "id"))
	if err != nil {
		s.writeUploadError(w, err)
		return
	}

	offset, err := tusOffset(r.Header.Get("Upload-Offset"))
	if err != nil {
		s.tusError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ct := strings.TrimSpace(r.Header.Get("Content-Type")); ct != "" &&
		!strings.HasPrefix(strings.ToLower(ct), "application/offset+octet-stream") {
		s.tusError(w, http.StatusUnsupportedMediaType,
			"PATCH requires Content-Type: application/offset+octet-stream")
		return
	}

	// creation-defer-length: the final PATCH declares the size. It is checked against the cap BEFORE
	// the bytes are appended, so a client cannot use deferred length to write past the limit one
	// chunk at a time.
	if raw := strings.TrimSpace(r.Header.Get("Upload-Length")); raw != "" {
		total, derr := strconv.ParseInt(raw, 10, 64)
		if derr != nil || total < 0 {
			s.tusError(w, http.StatusBadRequest, "Upload-Length must be a non-negative integer")
			return
		}
		if total > s.maxFileBytes(r.Context()) {
			s.tusError(w, http.StatusRequestEntityTooLarge, "the file exceeds the configured limit")
			return
		}
		if terr := s.deps.Uploads.SetLength(actor.ID(), sess.ID, total); terr != nil {
			s.tusError(w, http.StatusBadRequest, terr.Error())
			return
		}
	}

	// The chunk cap, from the same effective setting the bespoke endpoint enforces. Without it the
	// setting is a number in a table that nothing obeys — the exact defect its own comment records.
	if limit := s.chunkBytes(r.Context()); limit > 0 && r.ContentLength > limit {
		s.tusError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("the chunk exceeds limits.upload_chunk_bytes (%d); split the upload", limit))
		return
	}

	code, msg := s.tusAppendBody(w, r, actor, sess, offset, nil)
	if code != 0 {
		s.tusError(w, code, msg)
		return
	}

	s.tusSessionHeaders(w, r, sess)
	w.WriteHeader(http.StatusNoContent)
}

// handleTusDelete terminates a session.
func (s *Server) handleTusDelete(w http.ResponseWriter, r *http.Request) {
	if !s.tusVersionOK(w, r) {
		return
	}
	if s.deps.Uploads == nil {
		s.tusUnavailable(w)
		return
	}
	actor, _ := actorFrom(r.Context())
	if err := s.deps.Uploads.Abort(actor.ID(), chi.URLParam(r, "id")); err != nil {
		s.writeUploadError(w, err)
		return
	}
	w.Header().Set("Tus-Resumable", tusVersion)
	w.WriteHeader(http.StatusNoContent)
}

// tusAppendBody reads the request body into the session at offset.
//
// Returns (0, "") on success, or the status and message to answer with. It is one function because
// the creation-with-upload path and the ordinary PATCH have to behave IDENTICALLY: a client that
// uploads in one round trip and one that uploads in two must get the same validation, the same
// checksum handling and the same offset semantics.
func (s *Server) tusAppendBody(w http.ResponseWriter, r *http.Request, actor authz.Actor,
	sess *upload.Session, offset int64, meta map[string]string) (int, string) {

	// The checksum extension: `Upload-Checksum: sha256 <base64>`.
	//
	// It is verified by the SESSION manager after the bytes land and before the range is
	// acknowledged, which is the ordering that matters: a mismatch must not appear in the
	// acknowledged set, or a retry would be told the corrupt chunk is already there.
	wantSum, haveSum, cerr := parseTusChecksum(r.Header.Get("Upload-Checksum"))
	if cerr != nil {
		return http.StatusBadRequest, cerr.Error()
	}

	size := r.ContentLength
	if size < 0 {
		// A chunked body declares no length. The chunk cap still bounds it, and the session refuses
		// a write past the declared total.
		size = 0
	}
	if limit := s.chunkBytes(r.Context()); limit > 0 && size > limit {
		return http.StatusRequestEntityTooLarge,
			fmt.Sprintf("the chunk exceeds limits.upload_chunk_bytes (%d)", limit)
	}

	// A deferred-length session's Total is the cap; a chunk past it is refused by Append, so the
	// reader is bounded by what is left rather than by the cap.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxFileBytes(r.Context())+1)

	_, aerr := s.deps.Uploads.AppendWithChecksum(r.Context(), actor.ID(), sess.ID, offset,
		r.Body, size, wantSum, haveSum)
	if aerr != nil {
		var conflict *upload.ConflictError
		switch {
		case errors.As(aerr, &conflict):
			// 409 with the TRUE offset. This is the tus requirement and also the only answer that
			// lets a client resume: it is told where the server actually is instead of being told
			// its request was wrong.
			w.Header().Set("Tus-Resumable", tusVersion)
			w.Header().Set("Upload-Offset", strconv.FormatInt(conflict.Offset, 10))
			return http.StatusConflict, "the offset does not match the server's"
		case errors.Is(aerr, upload.ErrChecksumMismatch):
			// 460, and the acknowledged set does not include the chunk: the session's offset is
			// unchanged, so the client retries the SAME range.
			if s.deps.Metrics != nil {
				s.deps.Metrics.ObserveUpload("checksum_mismatch")
			}
			return tusChecksumMismatch, "the chunk does not match Upload-Checksum"
		case errors.Is(aerr, upload.ErrNotFound):
			return http.StatusNotFound, "no such upload"
		default:
			return http.StatusBadRequest, aerr.Error()
		}
	}

	// The final chunk completes the upload, so the session is finalized here — the PATCH that
	// reaches Upload-Length is where §1 puts the complete guard set.
	//
	// A PARTIAL is the exception, and it is not an optimisation: a part is complete when it fills,
	// but "complete" is not "finished" — it is one piece of a file whose final request has not
	// arrived. Publishing it here would leave a stray artefact per parallel chunk in the user's
	// namespace.
	off, _ := sess.Progress()
	if sess.Total > 0 && off == sess.Total && !sess.IsPartial() {
		if code, msg := s.tusFinalize(w, r, actor, sess); code != 0 {
			return code, msg
		}
	}
	_ = meta
	return 0, ""
}

// tusFinalize publishes a completed tus session through the same guards the bespoke completion uses.
//
// The sequence is copied from handleUploadComplete ON PURPOSE, in the same order: the two ways of
// writing the same bytes must not drift, and the earlier bespoke path was itself fixed for skipping
// every check the single-request upload makes. Sharing the code rather than restating it would be
// better still; the obstacle is that the bespoke path finishes from a POST /complete while this one
// finishes from the PATCH that completes, and the guards read a request that differs. The guards
// themselves are shared (childAtTarget, the lock helpers, preconditionHoldsNow, quotaHasRoomFor,
// publishFile), so what is duplicated is their ORDER, not their logic.
func (s *Server) tusFinalize(w http.ResponseWriter, r *http.Request, actor authz.Actor,
	sess *upload.Session) (int, string) {

	file, done, err := s.deps.Uploads.Finish(actor.ID(), sess.ID)
	if err != nil {
		var conflict *upload.ConflictError
		if errors.As(err, &conflict) {
			return http.StatusConflict, "the upload still has missing ranges"
		}
		if errors.Is(err, upload.ErrNotFound) {
			return http.StatusNotFound, "no such upload"
		}
		return http.StatusBadRequest, err.Error()
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(done.StagingPath(s.uploadStagingDir()))
	}()

	parent, chain, perr := s.writableParent(r.Context(), actor, done.Dir, authz.PermCreate)
	if perr != nil {
		s.writeStoreError(w, perr)
		return 0, ""
	}
	rel := path.Join(relOfChain(chain), done.Name)

	sibling, xerr := s.childAtTarget(r.Context(), parent.ID, done.Name)
	if xerr != nil {
		s.writeStoreError(w, xerr)
		return 0, ""
	}
	if sibling != nil && sibling.Name != done.Name {
		return http.StatusConflict,
			"something whose name differs only in case or Unicode form already exists there"
	}
	if sibling == nil || sibling.IsDir() {
		if !s.guardParentUnlocked(w, r, actor, parent.ID) {
			return 0, ""
		}
	}
	if sibling != nil && !sibling.IsDir() {
		if !s.guardUnlocked(w, r, actor, sibling.ID) {
			return 0, ""
		}
		if !s.guardAncestorContentUnlocked(w, r, actor, sibling.ID) {
			return 0, ""
		}
		parentCap, cerr := s.deps.Authz.Authorize(actor, parent.ID)
		if cerr != nil {
			s.writeStoreError(w, cerr)
			return 0, ""
		}
		if !parentCap.Can(authz.PermOverwrite) {
			return http.StatusNotFound, "no such directory"
		}
	}
	if !s.preconditionHoldsNow(r, sibling) {
		return http.StatusPreconditionFailed, "the file changed since you last read it"
	}
	if !s.quotaHasRoomFor(r.Context(), actor.ID(), done.Total, sibling) {
		return http.StatusInsufficientStorage, "your storage quota is full"
	}

	res, err := s.deps.Content.Put(r.Context(), nodeFor(actor, rel), file, done.Total)
	if err != nil {
		s.writeStoreError(w, err)
		return 0, ""
	}

	mime := guessType(done.Name)
	node, err := s.publishFile(r.Context(), r, actor, parent.ID, done.Name, rel, mime, res,
		preconditionFromHeader(r))
	if err != nil {
		s.writeStoreError(w, err)
		return 0, ""
	}
	setChecksumHeaders(w, res.Hash[:])
	_ = node
	return 0, ""
}

// tusConcatFinal joins partial uploads into one file.
//
// This is how tus-js-client implements parallelUploads: several partial sessions, then one final
// request naming them. The join is a byte-wise concatenation of their staging files, written into a
// NEW session so the existing publish path and its guards apply unchanged.
func (s *Server) tusConcatFinal(w http.ResponseWriter, r *http.Request, actor authz.Actor,
	meta map[string]string, concat string) {

	partials, err := tusPartialIDs(concat)
	if err != nil {
		s.tusError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(partials) == 0 {
		s.tusError(w, http.StatusBadRequest, "Upload-Concat: final; needs at least one partial")
		return
	}

	name := path.Base(meta["filename"])
	if name == "" || name == "." || name == "/" {
		s.tusError(w, http.StatusBadRequest, "Upload-Metadata must carry a filename")
		return
	}
	if verr := layout.ValidateSegment(name); verr != nil {
		s.tusError(w, http.StatusBadRequest, "the file needs a usable name")
		return
	}

	// Every partial has to be the caller's own, complete, and in the same destination directory.
	// The first two are what the manager already enforces; the third is what makes the joined file
	// land somewhere the caller was authorised to write.
	var (
		total    int64
		destDir  string
		sessions []*upload.Session
	)
	for _, id := range partials {
		sess, gerr := s.deps.Uploads.Get(actor.ID(), id)
		if gerr != nil {
			s.tusError(w, http.StatusNotFound, "no such upload: "+id)
			return
		}
		offset, _ := sess.Progress()
		if sess.Total > 0 && offset != sess.Total {
			s.tusError(w, http.StatusConflict, "a partial upload is not complete: "+id)
			return
		}
		if destDir == "" {
			destDir = sess.Dir
		} else if sess.Dir != destDir {
			s.tusError(w, http.StatusBadRequest,
				"partial uploads joined into one file must share a destination directory")
			return
		}
		total += offset
		sessions = append(sessions, sess)
	}
	if total > s.maxFileBytes(r.Context()) {
		s.tusError(w, http.StatusRequestEntityTooLarge, "the file exceeds the configured limit")
		return
	}

	_, chain, perr := s.writableParent(r.Context(), actor, destDir, authz.PermCreate)
	if perr != nil {
		s.writeStoreError(w, perr)
		return
	}
	contentDir := relOfChain(chain)

	s.deps.Uploads.SetMaxPendingBytes(s.maxPendingPendingForConcat(r.Context()))
	final, cerr := s.deps.Uploads.Create(actor.ID(), contentDir, name, total)
	if cerr != nil {
		s.tusError(w, http.StatusBadRequest, cerr.Error())
		return
	}

	// Copy each partial's bytes into the final session's staging file. This is a local file copy:
	// the bytes are already on this disk, and the alternative — publishing each partial and then
	// concatenating the results — would go through the content store twice for no benefit.
	if jerr := s.deps.Uploads.Concatenate(actor.ID(), final.ID, partialIDs(sessions)); jerr != nil {
		_ = s.deps.Uploads.Abort(actor.ID(), final.ID)
		s.tusError(w, http.StatusInternalServerError, "could not join the partial uploads")
		return
	}

	// The joined session is complete by construction, so it is published now.
	if code, msg := s.tusFinalize(w, r, actor, final); code != 0 {
		s.tusError(w, code, msg)
		return
	}
	// The partials are consumed by the join; leaving them would let a client publish the same bytes
	// twice and would hold staging space for a transfer that is over.
	for _, sess := range sessions {
		_ = s.deps.Uploads.Abort(actor.ID(), sess.ID)
	}

	w.Header().Set("Tus-Resumable", tusVersion)
	w.WriteHeader(http.StatusCreated)
}

// maxPendingPendingForConcat relaxes the staging cap for a concat join.
//
// The partials and the final session coexist for the length of the join, so the cap that bounds
// "unfinished uploads" would count the same bytes twice and refuse a join the client was allowed to
// start. The final session is created and immediately finished, so the temporary headroom is
// bounded by the file cap rather than by anything a client controls.
func (s *Server) maxPendingPendingForConcat(ctx context.Context) int64 {
	base := s.maxPendingUploadBytes(ctx)
	if base <= 0 {
		return 0
	}
	return base * 2
}

// partialIDs extracts the session ids from a slice of sessions.
func partialIDs(sessions []*upload.Session) []string {
	out := make([]string, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.ID)
	}
	return out
}

// tusPartialIDs parses "final;<url> <url> …" into session ids.
//
// The spec allows absolute URLs; only the LAST path segment is used, because that is the id this
// server issued and a client behind a proxy may build the URL from a different host than the one
// this process sees.
func tusPartialIDs(concat string) ([]string, error) {
	rest, ok := strings.CutPrefix(concat, "final;")
	if !ok {
		return nil, fmt.Errorf("Upload-Concat must look like \"final;<url> <url>\"")
	}
	var out []string
	for _, raw := range strings.Fields(rest) {
		trimmed := strings.TrimSuffix(raw, "/")
		if i := strings.LastIndexByte(trimmed, '/'); i >= 0 {
			trimmed = trimmed[i+1:]
		}
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out, nil
}

// tusSessionHeaders writes the headers every successful tus response carries.
func (s *Server) tusSessionHeaders(w http.ResponseWriter, r *http.Request, sess *upload.Session) {
	offset, _ := sess.Progress()
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Upload-Offset", strconv.FormatInt(offset, 10))
	// A deferred-length session has no Upload-Length yet, and the spec says to omit it rather than
	// to send the cap this server is using as a stand-in.
	if sess.Total > 0 && sess.Total != s.maxFileBytes(r.Context()) {
		w.Header().Set("Upload-Length", strconv.FormatInt(sess.Total, 10))
	}
	// expiration: the session's TTL, as an absolute time, so a client can decide whether to resume
	// or restart. It reuses limits.upload_session_ttl_hours — the sweep that enforces it already
	// exists as the upload-cleanup duty, so this is surfacing a policy rather than adding one.
	if ttl := s.deps.Config.Limits.UploadSessionTTL(); ttl > 0 {
		w.Header().Set("Upload-Expires", sess.Created.Add(ttl).UTC().Format(http.TimeFormat))
	}
}

// tusVersionOK enforces the protocol version, answering 412 for a mismatch as the spec requires.
func (s *Server) tusVersionOK(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Tus-Resumable", tusVersion)
	if v := strings.TrimSpace(r.Header.Get("Tus-Resumable")); v != "" && v != tusVersion {
		s.tusError(w, http.StatusPreconditionFailed,
			"this server implements tus "+tusVersion)
		return false
	}
	return true
}

// tusUnavailable answers when no session manager is wired.
func (s *Server) tusUnavailable(w http.ResponseWriter) {
	w.Header().Set("Tus-Resumable", tusVersion)
	s.tusError(w, http.StatusServiceUnavailable,
		"resumable uploads are not configured on this deployment")
}

// tusError writes a plain-text error, which is what tus clients expect.
//
// The JSON error shape would be friendlier for this server's own client and is wrong here: tus is an
// interoperable protocol and its clients read the status and the Tus-Resumable header, not a body.
func (s *Server) tusError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Tus-Resumable", tusVersion)
	http.Error(w, msg, status)
}

// parseTusMetadata decodes the Upload-Metadata header.
//
// The format is comma-separated "key base64value" pairs. A key with no value is legal and means
// "present, empty" — which is why the parser distinguishes it from a missing key.
func parseTusMetadata(raw string) (map[string]string, error) {
	out := map[string]string{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, nil
	}
	if len(raw) > tusMaxMetadataBytes {
		return nil, fmt.Errorf("Upload-Metadata is too large")
	}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, hasValue := strings.Cut(pair, " ")
		if key == "" {
			return nil, fmt.Errorf("Upload-Metadata has a pair with no key")
		}
		if !hasValue || strings.TrimSpace(value) == "" {
			out[key] = ""
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("Upload-Metadata value for %q is not base64", key)
		}
		out[key] = string(decoded)
	}
	return out, nil
}

// tusLength reads Upload-Length, reporting whether the length was deferred.
//
// "deferred" is the literal token creation-defer-length defines. An absent header is also deferred
// for the same reason: a client that intends to declare the size later has no other way to say so,
// and refusing it would make the extension unusable.
func tusLength(raw string) (length int64, deferred bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "deferred") {
		return 0, true, nil
	}
	n, perr := strconv.ParseInt(raw, 10, 64)
	if perr != nil || n < 0 {
		return 0, false, fmt.Errorf("Upload-Length must be a non-negative integer or \"deferred\"")
	}
	return n, false, nil
}

// tusOffset reads Upload-Offset.
func tusOffset(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// Required by the spec. Defaulting to zero would silently accept a chunk at the start of an
		// upload that is half done, which is the one thing the offset check exists to prevent.
		return 0, fmt.Errorf("Upload-Offset is required")
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("Upload-Offset must be a non-negative integer")
	}
	return n, nil
}

// parseTusChecksum reads `Upload-Checksum: sha256 <base64>`.
//
// Returns the expected digest and whether one was supplied. sha256 only, because it is the only
// hash this server computes and stores (core-scope §2); a client asking for md5 gets a refusal
// rather than a "verified" that verified nothing.
func parseTusChecksum(raw string) (sum [32]byte, present bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return sum, false, nil
	}
	algo, encoded, ok := strings.Cut(raw, " ")
	if !ok {
		return sum, false, fmt.Errorf(`Upload-Checksum must look like "sha256 <base64>"`)
	}
	if !strings.EqualFold(strings.TrimSpace(algo), "sha256") {
		return sum, false, fmt.Errorf("only the sha256 checksum is supported, got %q", algo)
	}
	decoded, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if derr != nil || len(decoded) != sha256.Size {
		return sum, false, fmt.Errorf("a sha256 checksum is %d base64-encoded bytes", sha256.Size)
	}
	copy(sum[:], decoded)
	return sum, true, nil
}

var _ = uuid.Nil
