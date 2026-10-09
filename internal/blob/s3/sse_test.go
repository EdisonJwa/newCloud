package s3

// Server-side encryption at rest (§9.4).
//
// The requirement asks for encryption at rest as a CONFIGURABLE policy. The failure mode that matters
// is not "encryption is off" — many object stores encrypt by default — it is a deployment that writes
// `sse: sse-kms` with no key, or a typo, and starts anyway: it believes it has encryption at rest
// because a line in a config file says so, and finds out otherwise during an audit. So the mode is
// validated at startup, an unknown value refuses to start, and the effective setting appears in the
// startup log.
//
// These tests check both halves: the configuration is rejected when wrong, and the header is actually
// SENT when right — proving the option was passed to the client rather than stored and forgotten,
// which is the defect class this codebase has produced most often.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/storage/s3fake"
)

func TestSSEModeIsValidatedAtStartup(t *testing.T) {
	cases := []struct {
		mode    string
		key     string
		wantErr string
	}{
		{mode: "", wantErr: ""},
		{mode: "sse-s3", wantErr: ""},
		{mode: "SSE-S3", wantErr: ""}, // case-insensitive
		{mode: "sse-kms", key: "arn:aws:kms:eu-west-1:123:key/abc", wantErr: ""},
		{mode: "sse-kms", wantErr: "sse_kms_key_id is required"},
		{mode: "aes256", wantErr: "unknown sse mode"},
		{mode: "sse-c", wantErr: "unknown sse mode"},
	}
	for _, c := range cases {
		_, err := newSSE(c.mode, c.key)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("mode %q key %q: %v", c.mode, c.key, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("mode %q key %q was accepted; a deployment would believe it had encryption "+
				"at rest", c.mode, c.key)
			continue
		}
		if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("mode %q: error %q does not mention %q", c.mode, err, c.wantErr)
		}
	}
}

// encryptedStore builds a store over its own fake endpoint, with the given SSE mode.
func encryptedStore(t *testing.T, mode, key string) (*Store, *s3fake.Server) {
	t.Helper()
	fake := s3fake.New("bucket")
	t.Cleanup(fake.Close)
	s, err := New(context.Background(), Config{
		Endpoint:    fake.Endpoint(),
		Bucket:      "bucket",
		AccessKey:   "fake-access",
		SecretKey:   "fake-secret",
		Prefix:      "t/" + randHex(4),
		SSEMode:     mode,
		SSEKMSKeyID: key,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return s, fake
}

// putContent stores content and returns the address and the stored object.
func putContent(t *testing.T, s *Store, fake *s3fake.Server, content []byte) s3fake.Obj {
	t.Helper()
	sum := sha256.Sum256(content)
	h := blob.Hash(sum)
	if err := s.Put(context.Background(), h, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("put: %v", err)
	}
	obj, ok := fake.Object("bucket", s.key(h))
	if !ok {
		t.Fatalf("the fake did not store the object under %q", s.key(h))
	}
	return obj
}

func TestSSES3PutsTheEncryptionHeader(t *testing.T) {
	s, fake := encryptedStore(t, "sse-s3", "")

	obj := putContent(t, s, fake, []byte("encrypt me"))
	if obj.SSE != "AES256" {
		t.Fatalf("the PUT did not ask for SSE-S3 (header %q). The setting was validated and then "+
			"not applied, which is worse than not having it: the log says encrypted and the object "+
			"is not.", obj.SSE)
	}
}

func TestSSEKMSNamesTheKey(t *testing.T) {
	const key = "arn:aws:kms:eu-west-1:123:key/abc"
	s, fake := encryptedStore(t, "sse-kms", key)

	obj := putContent(t, s, fake, []byte("kms"))
	if obj.SSE != "aws:kms" {
		t.Fatalf("the PUT did not ask for SSE-KMS (header %q)", obj.SSE)
	}
	if obj.KMSKey != key {
		t.Fatalf("the PUT named key %q, want %q", obj.KMSKey, key)
	}
}

func TestNoSSEMeansNoHeader(t *testing.T) {
	s, fake := encryptedStore(t, "", "")
	obj := putContent(t, s, fake, []byte("plain"))
	if obj.SSE != "" {
		t.Fatalf("an unconfigured store sent an encryption header (%q); the bucket policy should "+
			"decide, and the config says nothing", obj.SSE)
	}
}
