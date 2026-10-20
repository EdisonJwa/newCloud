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

// Put stores the payload under h.
//
// The bytes are hashed before they are uploaded. That costs one extra pass over the staging
// file and buys the property the interface promises: a mismatched payload never reaches the
// bucket, so a corruption or a truncated upload cannot poison the deduplication index for
// every user. Uploading first and checking afterwards would leave the bad object in place
// exactly when something is already going wrong.
func (s *Store) Put(ctx context.Context, h blob.Hash, r io.Reader, size int64) error {
	if _, err := s.Stat(ctx, h); err == nil {
		return nil // idempotent: the content address is already present
	} else if !errors.Is(err, blob.ErrNotFound) {
		return err
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
	_, err = s.client.PutObject(ctx, s.bucket, s.key(h), seeker, written, opts)
	if err != nil {
		return fmt.Errorf("blob/s3: put %s: %w", h, err)
	}

	// The upload was acknowledged. That is a claim about what the store now holds, and this is where
	// we check it rather than trusting it (WC-GAP-003).
	if err := s.verifyStored(ctx, h, written); err != nil {
		// A corrupted object must not be left at its own address: every later read would serve it,
		// the deduplication index would treat the address as present, and a re-upload of the correct
		// bytes would take the idempotent shortcut above and never repair it. Deleting makes the
		// next attempt upload again.
		//
		// The delete is best-effort: if it fails, the object stays and the integrity scrub is the
		// remaining detector. Reporting the verification failure is what matters — the caller must
		// not be told the write succeeded.
		if derr := s.client.RemoveObject(ctx, s.bucket, s.key(h), minio.RemoveObjectOptions{}); derr != nil {
			return fmt.Errorf("blob/s3: verify %s: %w (and the object could not be removed: %v)",
				h, err, derr)
		}
		return fmt.Errorf("blob/s3: verify %s: %w", h, err)
	}
	return nil
}

// verifyStored reads the object back and compares its hash to the address it is stored under.
//
// Written as a single pass over the object rather than a size check plus a hash, because the failure
// mode is a corrupted PART: the size can be exactly right while the bytes are wrong, which is
// precisely what a retried multipart upload produces.
func (s *Store) verifyStored(ctx context.Context, h blob.Hash, size int64) error {
	if !s.verifyAfterPut {
		return nil
	}
	if s.verifyMaxBytes > 0 && size > s.verifyMaxBytes {
		return nil
	}

	obj, err := s.client.GetObject(ctx, s.bucket, s.key(h), minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("read back: %w", err)
	}
	defer obj.Close()

	// GetObject is lazy; Stat forces the request so a missing or unreadable object fails here
	// rather than as a read error halfway through the hash.
	info, err := obj.Stat()
	if err != nil {
		return fmt.Errorf("read back: %w", translate(err))
	}
	if size >= 0 && info.Size != size {
		return fmt.Errorf("stored size is %d, uploaded %d", info.Size, size)
	}

	hasher := sha256.New()
	n, err := io.Copy(hasher, obj)
	if err != nil {
		return fmt.Errorf("read back: %w", err)
	}
	if n != size {
		return fmt.Errorf("stored object is %d bytes, uploaded %d", n, size)
	}
	var got blob.Hash
	copy(got[:], hasher.Sum(nil))
	if got != h {
		return blob.ErrHashMismatch
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
