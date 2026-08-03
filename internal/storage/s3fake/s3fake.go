package s3fake

// Package s3fake is an in-process object store that speaks enough of the S3 protocol for
// minio-go to talk to it.
//
// Why this exists: the S3 backend's conformance test was gated behind TEST_S3_ENDPOINT, so in
// every environment without a live MinIO it silently *skipped*. A skipped test is not evidence,
// and "the S3 backend passes the same suite as local disk" was therefore a claim nobody had
// checked here — the README even said so ("only verified against moto", which was no longer
// true because moto was gone).
//
// The alternative was requiring MinIO to run the test. That is fine for CI with a registry and
// useless for a developer offline, and it makes the test's coverage depend on whether a
// container happens to be up. A fake in the same package always runs.
//
// Scope: exactly the operations this codebase performs — BucketExists, StatObject, GetObject,
// PutObject (simple and multipart), RemoveObject, and ListObjectsV2 for the sweeper. It is
// deliberately NOT a general S3 implementation: anything outside that list returns an error
// rather than a plausible-looking wrong answer, so a backend change that starts using a new
// operation fails loudly here instead of passing against a fake that guessed.

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Obj is one stored object.
type Obj struct {
	Data    []byte
	ETag    string
	ModTime time.Time
	// Meta holds the user metadata headers, which the conformance suite does not read but a
	// caller might.
	Meta map[string]string
}

// Server is a fake S3 endpoint.
type Server struct {
	*httptest.Server

	mu      sync.RWMutex
	buckets map[string]map[string]*Obj // bucket -> key -> object
	// uploads holds in-progress multipart uploads: uploadID -> key -> parts.
	uploads map[string]*multipartUpload
	// FailNext makes the next matching request return an error, for testing the backend's
	// error handling.
	failNext map[string]bool
}

type multipartUpload struct {
	bucket string
	key    string
	parts  map[int][]byte
}

// New starts a fake S3 server with one bucket.
func New(bucket string) *Server {
	s := &Server{
		buckets:  map[string]map[string]*Obj{},
		uploads:  map[string]*multipartUpload{},
		failNext: map[string]bool{},
	}
	s.buckets[bucket] = map[string]*Obj{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Endpoint returns the host:port minio-go expects (no scheme).
func (s *Server) Endpoint() string {
	return strings.TrimPrefix(s.Server.URL, "http://")
}

// FailNextOn makes the next request whose method matches fail with a 500.
//
// It exists so the store's error paths can be exercised: "the backend returns an error" is a
// branch no happy-path conformance suite reaches.
func (s *Server) FailNextOn(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[method] = true
}

// ObjectCount reports how many objects a bucket holds.
func (s *Server) ObjectCount(bucket string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.buckets[bucket])
}

// handle dispatches one S3 request.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	// Path-style addressing: /<bucket>[/<key>]. minio-go uses it when the endpoint is not a
	// real AWS hostname.
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	s.mu.Lock()
	fail := s.failNext[r.Method]
	if fail {
		s.failNext[r.Method] = false
	}
	s.mu.Unlock()
	if fail {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`<Error><Code>InternalError</Code></Error>`))
		return
	}

	switch {
	// ---- bucket-level ----
	case bucket == "" && r.Method == http.MethodGet:
		// ListBuckets, used by discovery in some clients.
		s.listBuckets(w)
		return
	case key == "" && r.Method == http.MethodHead:
		s.headBucket(w, bucket)
		return
	case key == "" && r.Method == http.MethodGet && r.URL.Query().Get("location") != "":
		// GetBucketLocation: minio-go asks before signing some requests.
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
		return
	case key == "" && r.Method == http.MethodGet:
		// A bucket-level GET means different things depending on the query: BucketExists sends
		// a bare GET (minio-go treats 200 as "exists"), listing sends list-type=2. Answering the
		// bare form with a listing body would still be a 200, but it would be a lie about what
		// the request asked for.
		if r.URL.Query().Get("list-type") == "2" {
			s.listObjectsV2(w, r, bucket)
			return
		}
		s.headBucket(w, bucket)
		return

	// ---- multipart ----
	case key != "" && r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
		s.createMultipart(w, r, bucket, key)
		return
	case key != "" && r.Method == http.MethodPut && r.URL.Query().Get("uploadId") != "":
		s.uploadPart(w, r, bucket, key)
		return
	case key != "" && r.Method == http.MethodPost && r.URL.Query().Get("uploadId") != "":
		s.completeMultipart(w, r, bucket, key)
		return
	case key != "" && r.Method == http.MethodDelete && r.URL.Query().Get("uploadId") != "":
		s.abortMultipart(w, r, bucket, key)
		return

	// ---- object-level ----
	case key != "" && r.Method == http.MethodPut:
		s.putObject(w, r, bucket, key)
		return
	case key != "" && r.Method == http.MethodGet:
		s.getObject(w, r, bucket, key)
		return
	case key != "" && r.Method == http.MethodHead:
		s.headObject(w, r, bucket, key)
		return
	case key != "" && r.Method == http.MethodDelete:
		s.deleteObject(w, r, bucket, key)
		return
	}

	// Anything else is a deliberate error: a fake that answered plausibly would hide a backend
	// change that started using an operation this fake does not model.
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = fmt.Fprintf(w, `<Error><Code>NotImplemented</Code><Message>%s %s</Message></Error>`,
		r.Method, r.URL.Path)
}

func (s *Server) listBuckets(w http.ResponseWriter) {
	s.mu.RLock()
	names := make([]string, 0, len(s.buckets))
	for name := range s.buckets {
		names = append(names, name)
	}
	s.mu.RUnlock()
	sort.Strings(names)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Buckets>`)
	for _, n := range names {
		fmt.Fprintf(&b, `<Bucket><Name>%s</Name><CreationDate>2026-01-01T00:00:00.000Z</CreationDate></Bucket>`, n)
	}
	b.WriteString(`</Buckets></ListAllMyBucketsResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

// headBucket answers "does this bucket exist".
//
// minio-go's BucketExists accepts either a HEAD or a GET; it parses the body on a 200, so an
// empty response makes it report EOF rather than "exists". The body must be a valid (if empty)
// ListBucketResult for the GET form.
func (s *Server) headBucket(w http.ResponseWriter, bucket string) {
	s.mu.RLock()
	_, ok := s.buckets[bucket]
	s.mu.RUnlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchBucket</Code></Error>`))
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>` +
		bucket + `</Name><KeyCount>0</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated></ListBucketResult>`))
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// minio-go signs streaming uploads with AWS chunked encoding: the body is a sequence of
	// "<hex-size>;chunk-signature=<sig>\r\n<data>\r\n" frames rather than the plain bytes. A
	// server that stored the framed bytes would produce files that are subtly wrong in a way
	// only a content comparison reveals.
	data := raw
	if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") ||
		strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		if decoded, derr := decodeAWSChunked(raw); derr == nil {
			data = decoded
		} else {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `<Error><Code>InvalidRequest</Code><Message>%v</Message></Error>`, derr)
			return
		}
	}
	s.store(w, r, bucket, key, data)
}

// decodeAWSChunked removes AWS's `aws-chunked` transfer framing.
//
// The frame format is: hex-length[;chunk-signature=sig]\r\npayload\r\n, repeated, terminated by
// a zero-length frame. Only the payload bytes belong in the object.
func decodeAWSChunked(b []byte) ([]byte, error) {
	var out []byte
	for {
		// Read the header line up to \r\n.
		idx := bytes.Index(b, []byte("\r\n"))
		if idx < 0 {
			return out, nil
		}
		header := string(b[:idx])
		b = b[idx+2:]

		sizeStr, _, _ := strings.Cut(header, ";")
		size, err := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("bad chunk header %q: %w", header, err)
		}
		if size == 0 {
			return out, nil
		}
		if int64(len(b)) < size {
			return nil, fmt.Errorf("chunk claims %d bytes, %d remain", size, len(b))
		}
		out = append(out, b[:size]...)
		b = b[size:]
		// Trailing \r\n after the payload.
		b = bytes.TrimPrefix(b, []byte("\r\n"))
	}
}

// store records an object and writes the response headers a client expects.
func (s *Server) store(w http.ResponseWriter, r *http.Request, bucket, key string, data []byte) {
	sum := md5.Sum(data)
	etag := hex.EncodeToString(sum[:])

	meta := map[string]string{}
	for k, v := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") && len(v) > 0 {
			meta[strings.ToLower(k)] = v[0]
		}
	}

	s.mu.Lock()
	b, ok := s.buckets[bucket]
	if !ok {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchBucket</Code></Error>`))
		return
	}
	b[key] = &Obj{Data: data, ETag: etag, ModTime: time.Now().UTC(), Meta: meta}
	s.mu.Unlock()

	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	s.mu.RLock()
	b, ok := s.buckets[bucket]
	if !ok {
		s.mu.RUnlock()
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchBucket</Code></Error>`))
		return
	}
	obj, ok := b[key]
	if !ok {
		s.mu.RUnlock()
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
		return
	}
	data := obj.Data
	etag := obj.ETag
	mod := obj.ModTime
	s.mu.RUnlock()

	// Range support: minio-go's Object requests ranges when the caller seeks.
	if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
		if start, end, ok := parseRange(rangeHdr, int64(len(data))); ok {
			// A 206 must carry the same object headers as a 200: minio-go's Object re-reads
			// Stat() on seek, and a missing Last-Modified makes the client report an
			// unparseable date rather than the length it asked for.
			w.Header().Set("ETag", `"`+etag+`"`)
			w.Header().Set("Last-Modified", mod.UTC().Format(http.TimeFormat))
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[start : end+1])
			return
		}
	}

	w.Header().Set("ETag", `"`+etag+`"`)
	// http.TimeFormat is "Mon, 02 Jan 2006 15:04:05 GMT", which is what S3 sends and what the
	// conformance suite parses. Using any other layout yields an empty header and a confusing
	// "Last-Modified time format is invalid" from the client.
	w.Header().Set("Last-Modified", mod.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// parseRange parses "bytes=start-end" (both may be omitted).
func parseRange(h string, size int64) (int64, int64, bool) {
	h = strings.TrimPrefix(h, "bytes=")
	a, b, ok := strings.Cut(h, "-")
	if !ok {
		return 0, 0, false
	}
	var start, end int64
	if a == "" {
		// Suffix range: the last N bytes.
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		start = size - n
		if start < 0 {
			start = 0
		}
		end = size - 1
	} else {
		var err error
		start, err = strconv.ParseInt(a, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		if b == "" {
			end = size - 1
		} else {
			end, err = strconv.ParseInt(b, 10, 64)
			if err != nil {
				return 0, 0, false
			}
		}
	}
	if end >= size {
		end = size - 1
	}
	if start > end || start < 0 {
		return 0, 0, false
	}
	return start, end, true
}

func (s *Server) headObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	s.mu.RLock()
	b, ok := s.buckets[bucket]
	if !ok {
		s.mu.RUnlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	obj, ok := b[key]
	if !ok {
		s.mu.RUnlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	size, etag, mod := len(obj.Data), obj.ETag, obj.ModTime
	s.mu.RUnlock()

	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Last-Modified", mod.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.Itoa(size))
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	s.mu.Lock()
	if b, ok := s.buckets[bucket]; ok {
		delete(b, key)
	}
	s.mu.Unlock()
	// S3 answers 204 for a delete regardless of existence.
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listObjectsV2(w http.ResponseWriter, r *http.Request, bucket string) {
	prefix := r.URL.Query().Get("prefix")

	s.mu.RLock()
	b, ok := s.buckets[bucket]
	if !ok {
		s.mu.RUnlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	type item struct {
		key  string
		size int
	}
	var items []item
	for k, obj := range b {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			items = append(items, item{k, len(obj.Data)})
		}
	}
	s.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })

	var out strings.Builder
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	fmt.Fprintf(&out, `<Name>%s</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>`,
		bucket, prefix, len(items))
	for _, it := range items {
		fmt.Fprintf(&out, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-01-01T00:00:00.000Z</LastModified><ETag>&quot;x&quot;</ETag><StorageClass>STANDARD</StorageClass></Contents>`,
			it.key, it.size)
	}
	out.WriteString(`</ListBucketResult>`)

	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(out.String()))
}

// ---- multipart ----

type initiateResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

func (s *Server) createMultipart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	s.mu.Lock()
	if _, ok := s.buckets[bucket]; !ok {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	id := fmt.Sprintf("upload-%d", time.Now().UnixNano())
	s.uploads[id] = &multipartUpload{bucket: bucket, key: key, parts: map[int][]byte{}}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(initiateResult{Bucket: bucket, Key: key, UploadID: id})
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	id := r.URL.Query().Get("uploadId")
	partNum, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") ||
		strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		if decoded, derr := decodeAWSChunked(data); derr == nil {
			data = decoded
		}
	}

	s.mu.Lock()
	up, ok := s.uploads[id]
	if ok {
		up.parts[partNum] = data
	}
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchUpload</Code></Error>`))
		return
	}

	sum := md5.Sum(data)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.WriteHeader(http.StatusOK)
}

type completeResult struct {
	XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
	Bucket  string   `xml:"Bucket"`
	Key     string   `xml:"Key"`
	ETag    string   `xml:"ETag"`
}

func (s *Server) completeMultipart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	id := r.URL.Query().Get("uploadId")

	// The client declares the part order in the request body. Honouring it rather than sorting
	// by part number is what S3 does, and it is the only correct thing when parts are uploaded
	// concurrently: minio-go uses a worker pool and the part numbers are assigned per chunk, so
	// both the set of completed parts and their order come from this document.
	requested, perr := parseCompletedParts(r.Body)

	s.mu.Lock()
	up, ok := s.uploads[id]
	var data []byte
	if ok {
		nums := requested
		if len(nums) == 0 {
			// Fall back to numeric order when the body carries no part list.
			for n := range up.parts {
				nums = append(nums, n)
			}
			sort.Ints(nums)
		}
		for _, n := range nums {
			data = append(data, up.parts[n]...)
		}
		delete(s.uploads, id)
	}
	s.mu.Unlock()

	if perr != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `<Error><Code>MalformedXML</Code><Message>%v</Message></Error>`, perr)
		return
	}

	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchUpload</Code></Error>`))
		return
	}

	sum := md5.Sum(data)
	etag := hex.EncodeToString(sum[:])

	s.mu.Lock()
	s.buckets[bucket][key] = &Obj{Data: data, ETag: etag, ModTime: time.Now().UTC(), Meta: map[string]string{}}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(completeResult{Bucket: bucket, Key: key, ETag: etag})
}

func (s *Server) abortMultipart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	id := r.URL.Query().Get("uploadId")
	s.mu.Lock()
	delete(s.uploads, id)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// PutDirect stores an object without going through HTTP, for test setup.
func (s *Server) PutDirect(bucket, key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buckets[bucket] == nil {
		s.buckets[bucket] = map[string]*Obj{}
	}
	sum := md5.Sum(data)
	s.buckets[bucket][key] = &Obj{
		Data: data, ETag: hex.EncodeToString(sum[:]),
		ModTime: time.Now().UTC(), Meta: map[string]string{},
	}
}

// GetDirect returns an object's bytes, for assertions.
func (s *Server) GetDirect(bucket, key string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.buckets[bucket]
	if !ok {
		return nil, false
	}
	obj, ok := b[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), obj.Data...), true
}

// Keys returns every key in a bucket, sorted.
func (s *Server) Keys(bucket string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.buckets[bucket]))
	for k := range s.buckets[bucket] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseCompletedParts reads <CompleteMultipartUpload><Part><PartNumber>N</PartNumber>...
//
// The order of the Part elements is the order the object is assembled in.
func parseCompletedParts(body io.Reader) ([]int, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}

	var doc struct {
		Parts []struct {
			PartNumber int `xml:"PartNumber"`
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := make([]int, 0, len(doc.Parts))
	for _, p := range doc.Parts {
		out = append(out, p.PartNumber)
	}
	return out, nil
}
