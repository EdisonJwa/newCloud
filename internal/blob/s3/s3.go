// Package s3 stores content-addressed blobs in an S3-compatible object store.
//
// It implements blob.Store and nothing else: multipart policy, versions, quotas and
// deduplication all live above this layer, which is what keeps a third backend small.
//
// The client is minio-go rather than the AWS SDK. Both work against AWS, but the audience for
// a self-hosted server is mostly MinIO, Ceph, R2, B2 and Wasabi, and minio-go is built for
// exactly that: endpoint plus path-style addressing, no region-resolution surprises, and a far
// smaller dependency tree than aws-sdk-go-v2. It is also the client rclone uses.
//
// Consistency note: no attempt is made to repair a multipart upload interrupted by a crash.
// minio-go uploads large objects as a managed multipart and an aborted upload leaves no
// visible object, so a content address either exists completely or not at all — the only
// property the layer above needs.
package s3

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/encrypt"

	"github.com/EdisonJwa/newCloud/internal/blob"
)

// Config is the subset of connection settings this store needs.
type Config struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	// Secure selects https. Default true: sending object data in the clear should be an
	// explicit decision, not the default.
	Secure bool
	// Prefix namespaces keys inside a bucket that may be shared with other applications.
	Prefix string
	// SSEMode selects server-side encryption at rest (§9.4): "", "sse-s3" or "sse-kms".
	//
	// Empty means "whatever the bucket policy does", which is a legitimate deployment choice — many
	// object stores encrypt by default and adding a header changes nothing — but it must be a
	// choice. The requirement asks for encryption at rest as a CONFIGURABLE policy, and a deployment
	// cannot audit what it cannot see configured.
	SSEMode string
	// SSEKMSKeyID names the KMS key for sse-kms. Required for that mode, ignored otherwise.
	SSEKMSKeyID string
	// VerifyAfterPut reads the object back and re-hashes it before reporting success
	// (WC-GAP-003, INV-03).
	//
	// Why this exists: Put hashes the INPUT before uploading, which proves the payload matched its
	// address when it left this process — not that those bytes are what the store now holds. The
	// client (minio-go v7.4.1) has a known race on the multipart retry path where a retried part
	// can be assembled from a buffer being rewritten concurrently, and the observable result is a
	// corrupted part that lands successfully at a content-addressed key. Without a read-back, the
	// only detector is the periodic scrub, which is opt-in and samples.
	//
	// Off by default, and that is a deliberate trade rather than an oversight. Verification doubles
	// the bytes read from the store and adds a round trip to every upload; on a large deployment
	// that is a real cost for a narrow window. Leave it on wherever the store is not trusted to
	// deliver what it acknowledged, which for a self-hosted deployment over a smaller object store
	// is most of the time.
	VerifyAfterPut bool
	// VerifyMaxBytes skips the read-back above this size. Zero verifies everything.
	//
	// The largest uploads are the ones most likely to hit the multipart retry path, so this is not
	// a "verify only the cheap ones" knob: set it only when the egress cost of re-reading a very
	// large object exceeds its value, and record that the deployment accepted the gap.
	VerifyMaxBytes int64
}

// Store is an S3-backed blob store.
type Store struct {
	client *minio.Client
	bucket string
	prefix string
	// sse is the encryption applied to every object this store writes. Nil means the header is
	// omitted and the bucket's own policy decides.
	sse encrypt.ServerSide
	// verifyAfterPut and verifyMaxBytes control the post-upload read-back (WC-GAP-003).
	verifyAfterPut bool
	verifyMaxBytes int64
}

// New connects and verifies the bucket is reachable.
//
// The startup check is deliberate: an unreachable bucket discovered on the first upload is an
// outage, while the same failure at boot is a configuration error somebody can fix.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("blob/s3: endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("blob/s3: bucket is required")
	}

	creds := credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	if cfg.AccessKey == "" {
		// Fall back to the standard chain: environment, then the shared credentials file, then
		// the instance role. A deployment on EC2 or EKS should not have to write keys into a
		// config file to be able to start.
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.FileAWSCredentials{},
			&credentials.IAM{},
		})
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  creds,
		Secure: cfg.Secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("blob/s3: client: %w", err)
	}

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("blob/s3: bucket %q: %w", cfg.Bucket, err)
	}
	if !exists {
		return nil, fmt.Errorf("blob/s3: bucket %q does not exist", cfg.Bucket)
	}

	sse, err := newSSE(cfg.SSEMode, cfg.SSEKMSKeyID)
	if err != nil {
		return nil, err
	}

	return &Store{
		client:         client,
		bucket:         cfg.Bucket,
		prefix:         strings.Trim(cfg.Prefix, "/"),
		sse:            sse,
		verifyAfterPut: cfg.VerifyAfterPut,
		verifyMaxBytes: cfg.VerifyMaxBytes,
	}, nil
}

// newSSE turns the configured mode into the encryption applied to every write.
//
// Validated here rather than at config load: the mode is meaningless without a key for sse-kms, and
// an unknown value silently falling back to "no encryption" is the failure this check exists to
// prevent — a deployment would believe it had encryption at rest because it wrote a line in a file.
func newSSE(mode, kmsKeyID string) (encrypt.ServerSide, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "":
		return nil, nil
	case "sse-s3":
		return encrypt.NewSSE(), nil
	case "sse-kms":
		if kmsKeyID == "" {
			return nil, errors.New("blob/s3: sse_kms_key_id is required when sse is sse-kms")
		}
		sse, err := encrypt.NewSSEKMS(kmsKeyID, nil)
		if err != nil {
			return nil, fmt.Errorf("blob/s3: sse-kms: %w", err)
		}
		return sse, nil
	default:
		return nil, fmt.Errorf("blob/s3: unknown sse mode %q (want \"\", \"sse-s3\" or \"sse-kms\") "+
			" — refusing to start rather than writing unencrypted objects a deployment believes "+
			"are encrypted", mode)
	}
}

// key maps a content address to an object key.
//
// The two-level shard keeps listings and any future lifecycle rule from putting every object
// in one prefix. The full hash is the leaf name, so the key stays a direct function of the
// content address rather than a derived identifier.
func (s *Store) key(h blob.Hash) string {
	hex := h.String()
	if s.prefix == "" {
		return path.Join(hex[0:2], hex[2:4], hex)
	}
	return path.Join(s.prefix, hex[0:2], hex[2:4], hex)
}

// ErrVerifyUnavailable reports that a stored object could not be read back, so nothing is known
// about whether its bytes are the ones that were uploaded (NC-RR-003).
//
// The distinction this type exists to carry is the whole of NC-RR-003:
//
//	Corrupt — the object was read and its bytes do NOT hash to the address. That is evidence.
//	Unavailable — the readback failed (timeout, 5xx, 403, a transport reset). That is the ABSENCE
//	              of evidence, and the two must not be handled the same way.
//
// The defect was that every verification error triggered RemoveObject, so a transient GET failure
// deleted a blob that had been written successfully. With several writers deduplicating onto one
// content address, the object may also have been placed by somebody else entirely, which makes
// deleting it on an inconclusive read an unqualified destructive act on another caller's data.
var ErrVerifyUnavailable = errors.New("blob/s3: stored object could not be verified")

// verifyMismatch marks a DEFINITIVE verification failure: the object was read and its content does
// not match its address, or its metadata cannot possibly describe a correct object.
//
// It wraps blob.ErrHashMismatch so that a caller which only cares "the bytes and the address
// disagree" keeps working unchanged, while a caller that must distinguish evidence from the absence
// of it (Put's cleanup decision) can ask the more specific question via errors.As.
type verifyMismatch struct{ reason string }

func (e *verifyMismatch) Error() string {
	return "blob/s3: stored object is corrupt: " + e.reason + ": " + blob.ErrHashMismatch.Error()
}

// Unwrap keeps blob.ErrHashMismatch in the chain for existing callers and tests.
func (e *verifyMismatch) Unwrap() error { return blob.ErrHashMismatch }

// isMismatch reports whether err is evidence of corruption rather than absence of evidence.
func isMismatch(err error) bool {
	var m *verifyMismatch
	return errors.As(err, &m)
}

// Put stores the payload under h.
//
// The bytes are hashed before they are uploaded. That costs one extra pass over the staging
// file and buys the property the interface promises: a mismatched payload never reaches the
// bucket, so a corruption or a truncated upload cannot poison the deduplication index for
// every user. Uploading first and checking afterwards would leave the bad object in place
// exactly when something is already going wrong.
func (s *Store) Put(ctx context.Context, h blob.Hash, r io.Reader, size int64) error {
	exists, err := s.statExisting(ctx, h)
	if err != nil {
		return err
	}
	if exists {
		// Idempotent: the content address is already present (NC-RR-004).
		//
		// Returning here unconditionally was a defect when verification is enabled. The address
		// being PRESENT says nothing about its bytes: an older or concurrently corrupted object
		// sits under the same key, and the upload of correct bytes cannot repair it. A dedup hit
		// is a place where a deployment that asked for integrity checking should get it, not a
		// shortcut past it. When verification is off this is still the cheap, correct answer.
		return s.verifyExisting(ctx, h)
	}

	body := r
	cleanup := func() {}
	if _, ok := r.(io.ReadSeeker); !ok {
		// An unseekable reader has to be staged: the hash is needed before the upload starts.
		staged, done, err := stage(ctx, r)
		if err != nil {
			return err
		}
		body, cleanup = staged, done
	}
	defer cleanup()

	seeker, ok := body.(io.ReadSeeker)
	if !ok {
		return errors.New("blob/s3: staged reader is not seekable")
	}

	// Hash and upload must see the same bytes, so the starting offset is remembered rather
	// than assumed to be zero.
	start, err := seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("blob/s3: locate: %w", err)
	}

	hasher := sha256.New()
	written, err := io.Copy(hasher, seeker)
	if err != nil {
		return fmt.Errorf("blob/s3: hash: %w", err)
	}
	if size >= 0 && written != size {
		return fmt.Errorf("blob/s3: size mismatch: declared %d, got %d", size, written)
	}
	var got blob.Hash
	copy(got[:], hasher.Sum(nil))
	if got != h {
		return blob.ErrHashMismatch
	}

	if _, err := seeker.Seek(start, io.SeekStart); err != nil {
		return fmt.Errorf("blob/s3: rewind: %w", err)
	}

	opts := minio.PutObjectOptions{
		ContentType: "application/octet-stream",
		// The key is a hash, so the bytes behind it never change.
		UserMetadata: map[string]string{"sha256": h.String()},
	}
	if s.sse != nil {
		opts.ServerSideEncryption = s.sse
	}
	upload, err := s.client.PutObject(ctx, s.bucket, s.key(h), seeker, written, opts)
	if err != nil {
		return fmt.Errorf("blob/s3: put %s: %w", h, err)
	}

	// The upload was acknowledged. That is a claim about what the store now holds, and this is where
	// we check it rather than trusting it (WC-GAP-003).
	//
	// The two failure classes are handled differently on purpose (NC-RR-003):
	//
	//	A corrupt object is evidence. It must not be left at its own address: every later read
	//	would serve it, the dedup index would treat the address as present, and a re-upload of
	//	the correct bytes would take the idempotent path and never repair it.
	//
	//	An unverifiable object proves nothing. A GET that timed out, a 5xx, a permissions blip or a
	//	cancelled request says the readback did not happen, not that the bytes are wrong. Deleting
	//	on that basis destroys a valid blob — and with concurrent dedup writers the object may not
	//	even be the one this call uploaded. It is reported and the object is LEFT ALONE; the
	//	periodic scrub remains the detector for corruption nobody managed to observe.
	if err := s.verifyStored(ctx, h, written); err != nil {
		if !isMismatch(err) {
			return fmt.Errorf("blob/s3: verify %s: object left in place because the readback was "+
				"inconclusive: %w", h, err)
		}
		// The mismatch is proven, but ownership is not (PRG-006).
		//
		// Between this call's PUT and this removal, another writer deduplicating onto the same
		// content address may have REPLACED the object with its own copy — which, if that writer's
		// bytes are correct, is a valid object this call would now delete. Content addressing makes
		// that rare (every writer sends the same bytes), but "rare" is not "impossible": the two
		// copies can differ precisely when one of them is corrupt, which is the situation here.
		//
		// So the removal is guarded by the ETag of the object this call wrote. If the stored ETag no
		// longer matches, somebody else owns the key now and this call must not touch it. The check
		// narrows the window to Stat->Delete rather than eliminating it, because S3 offers no
		// conditional DELETE without object versioning; that residual window is documented in
		// docs/guides/deployment.md rather than papered over.
		switch derr := s.removeIfStillOurs(ctx, h, upload.ETag); {
		case errors.Is(derr, errObjectReplaced):
			return fmt.Errorf("blob/s3: verify %s: %w (the object was replaced by another writer "+
				"after verification, so it was NOT removed; the integrity scrub will re-check it)",
				h, err)
		case derr != nil:
			return fmt.Errorf("blob/s3: verify %s: %w (and the object could not be removed: %v)",
				h, err, derr)
		}
		return fmt.Errorf("blob/s3: verify %s: %w", h, err)
	}
	return nil
}

// errObjectReplaced reports that the object at a key is no longer the one this call wrote.
var errObjectReplaced = errors.New("blob/s3: the object was replaced by another writer")

// removeIfStillOurs deletes the object at h only if it is still the version this call uploaded,
// identified by the ETag the store returned from PutObject.
//
// This is the "establish ownership before destructive cleanup" requirement from the audit's PRG-006.
// An unconditional RemoveObject is unsafe under concurrent deduplicating writers: the object at a
// hash key may have been replaced between verification and deletion, and deleting another writer's
// freshly uploaded object would destroy valid content in the middle of someone else's upload.
//
// An empty wantETag means the store did not report one (some S3-compatible servers omit it); in
// that case there is nothing to compare, so the removal proceeds and the caller's log records the
// weaker guarantee. Refusing to clean up at all would leave a proven-corrupt object at its address
// for every future reader, which is the worse failure.
func (s *Store) removeIfStillOurs(ctx context.Context, h blob.Hash, wantETag string) error {
	if wantETag != "" {
		info, err := s.client.StatObject(ctx, s.bucket, s.key(h), minio.StatObjectOptions{})
		if err != nil {
			return fmt.Errorf("stat before removal: %w", translate(err))
		}
		if got := strings.Trim(info.ETag, `"`); got != strings.Trim(wantETag, `"`) {
			return errObjectReplaced
		}
	}
	return s.client.RemoveObject(ctx, s.bucket, s.key(h), minio.RemoveObjectOptions{})
}

// statExisting reports whether an object already exists at h.
//
// Split out so Put's two paths read clearly: the distinction between "absent, so upload it" and
// "present, so decide whether to verify it" is the whole of NC-RR-004, and it is easier to see when
// it is a named function returning a bool rather than an if/else-with-assignment.
func (s *Store) statExisting(ctx context.Context, h blob.Hash) (bool, error) {
	if _, err := s.Stat(ctx, h); err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return false, nil
		}
		// An inconclusive Stat is not "absent". Uploading over it is safe (the key is immutable),
		// but reporting the store as broken is more useful than guessing.
		return false, err
	}
	return true, nil
}

// verifyExisting applies the integrity contract to a deduplication hit (NC-RR-004).
//
// When verification is disabled this is the historical cheap no-op, and that is a documented
// trade rather than an oversight: re-reading every duplicate upload doubles egress on the path
// with the most traffic.
//
// When it is enabled, a Hit is checked like any other write, and for the same reason. The
// alternative — trusting the key because it exists — means the ONE state a verifying deployment
// most needs to detect (a corrupt object already at an address) is the state that verification
// skips. A mismatch is an error; a successful check permits the caller's write to be treated as
// done. An inconclusive readback is still not evidence of corruption, so it is reported without
// any destructive action, and a mismatch is NOT deleted here: unlike the post-PUT path this call
// did not create the object, so it cannot know who else references it.
func (s *Store) verifyExisting(ctx context.Context, h blob.Hash) error {
	if !s.verifyAfterPut {
		return nil
	}
	info, err := s.Stat(ctx, h)
	if err != nil {
		return fmt.Errorf("blob/s3: verify existing %s: %w", h, err)
	}
	if s.verifyMaxBytes > 0 && info.Size > s.verifyMaxBytes {
		return nil
	}
	if err := s.verifyStored(ctx, h, info.Size); err != nil {
		return fmt.Errorf("blob/s3: existing object %s failed verification: %w", h, err)
	}
	return nil
}

// verifyStored reads the object back and compares its hash to the address it is stored under.
//
// Written as a single pass over the object rather than a size check plus a hash, because the failure
// mode is a corrupted PART: the size can be exactly right while the bytes are wrong, which is
// precisely what a retried multipart upload produces.
//
// The returned error distinguishes the two outcomes NC-RR-003 is about:
//
//	*verifyMismatch          the object was read and its content is wrong — evidence of corruption.
//	ErrVerifyUnavailable     the object could not be read — no evidence either way.
//
// Every "could not read" path returns the second, and every "read it and the bytes are wrong" path
// returns the first. Callers act on the difference; a caller that treats them alike is the defect
// this type was introduced to make impossible to write by accident.
func (s *Store) verifyStored(ctx context.Context, h blob.Hash, size int64) error {
	if !s.verifyAfterPut {
		return nil
	}
	if s.verifyMaxBytes > 0 && size > s.verifyMaxBytes {
		return nil
	}

	obj, err := s.client.GetObject(ctx, s.bucket, s.key(h), minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("%w: open readback: %v", ErrVerifyUnavailable, translate(err))
	}
	defer obj.Close()

	// GetObject is lazy; Stat forces the request so a missing or unreadable object fails here
	// rather than as a read error halfway through the hash. Everything Stat can report is a
	// readback problem, never proof about content.
	info, err := obj.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat readback: %v", ErrVerifyUnavailable, translate(err))
	}
	if size >= 0 && info.Size != size {
		return &verifyMismatch{reason: fmt.Sprintf("stored size is %d, uploaded %d", info.Size, size)}
	}

	hasher := sha256.New()
	n, err := io.Copy(hasher, obj)
	if err != nil {
		// A partially delivered body: the transport failed mid-read. That is the same absence of
		// evidence as a failed Stat, NOT a truncated object — a short read surfaces as an error,
		// while a genuinely short object is caught by the size comparison above.
		return fmt.Errorf("%w: read back: %v", ErrVerifyUnavailable, err)
	}
	if n != info.Size {
		return &verifyMismatch{reason: fmt.Sprintf("stored object is %d bytes, its metadata says %d",
			n, info.Size)}
	}
	var got blob.Hash
	copy(got[:], hasher.Sum(nil))
	if got != h {
		return &verifyMismatch{reason: fmt.Sprintf("content hashes to %s, address is %s", got, h)}
	}
	return nil
}

// Open returns a seekable reader over the blob.
//
// minio.Object already implements io.ReadSeeker and turns a Seek into a ranged GET, which is
// precisely what serving HTTP Range requests needs. Writing a ranged reader here would be
// reimplementing the one thing the library does correctly.
func (s *Store) Open(ctx context.Context, h blob.Hash) (io.ReadSeekCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.key(h), minio.GetObjectOptions{})
	if err != nil {
		return nil, translate(err)
	}
	// GetObject is lazy: the first request happens on the first read, so a missing object has
	// to be detected here rather than at call time.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, translate(err)
	}
	return obj, nil
}

// Delete removes the blob.
func (s *Store) Delete(ctx context.Context, h blob.Hash) error {
	// RemoveObject is idempotent on S3, which makes "delete a missing object" look like
	// success. The interface distinguishes the two, so the existence check stays.
	if _, err := s.Stat(ctx, h); err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucket, s.key(h), minio.RemoveObjectOptions{}); err != nil {
		return translate(err)
	}
	return nil
}

// Stat returns the size without transferring the content.
func (s *Store) Stat(ctx context.Context, h blob.Hash) (blob.Info, error) {
	info, err := s.client.StatObject(ctx, s.bucket, s.key(h), minio.StatObjectOptions{})
	if err != nil {
		return blob.Info{}, translate(err)
	}
	return blob.Info{Hash: h, Size: info.Size}, nil
}

// translate maps the client's error shapes onto the package's sentinel.
func translate(err error) error {
	if err == nil {
		return nil
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		switch resp.Code {
		case "NoSuchKey", "NoSuchObject", "NotFound":
			return blob.ErrNotFound
		}
	}
	if errors.Is(err, io.EOF) {
		return blob.ErrNotFound
	}
	return fmt.Errorf("blob/s3: %w", err)
}

// stage spills an unseekable reader to a temporary file so it can be hashed and then uploaded.
func stage(ctx context.Context, r io.Reader) (*os.File, func(), error) {
	f, err := os.CreateTemp("", "newcloud-s3-*")
	if err != nil {
		return nil, nil, fmt.Errorf("blob/s3: create temp: %w", err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	if _, err := io.Copy(f, &ctxReader{ctx: ctx, r: r}); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("blob/s3: stage: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("blob/s3: rewind temp: %w", err)
	}
	return f, cleanup, nil
}

// ctxReader aborts a copy when the request is cancelled, so a client that hangs up does not
// leave a goroutine reading a large body to completion.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
