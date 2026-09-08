package thumbnail_test

// §6.3: 处理队列可重试，失败结果有冷却和回收策略；避免持续重试恶意文件耗尽资源.
//
// The generator already bounded one attempt three ways: a semaphore for concurrency, a deadline for
// a crafted file, and a pixel cap that refuses the decode outright. None of them bounds the NUMBER
// of attempts. A file that always fails was therefore decoded again on every request, forever —
// the concurrency limit turned an unbounded rate into a bounded one but did not make it finite.
//
// The cooldown is keyed by content address, which is why it is safe: replacing a broken file
// produces different bytes, hence a different address, hence an immediate fresh attempt. A long
// window never hides a fixed file.

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/thumbnail"
)

// countingReader records how many times the decoder actually read the body.
type countingReader struct {
	r    *bytes.Reader
	read *int
}

func (c *countingReader) Read(p []byte) (int, error) {
	*c.read++
	return c.r.Read(p)
}

func (c *countingReader) Seek(off int64, whence int) (int64, error) {
	return c.r.Seek(off, whence)
}

func newCounting(body string) (*countingReader, *int) {
	n := 0
	return &countingReader{r: bytes.NewReader([]byte(body)), read: &n}, &n
}

// TestAFailedDecodeIsNotRetriedOnEveryRequest is the cooldown property.
func TestAFailedDecodeIsNotRetriedOnEveryRequest(t *testing.T) {
	g := thumbnail.New()

	// A PNG header with a truncated, undecodable body: the kind check passes, the decode fails.
	body := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32)
	var hash [32]byte
	hash[0] = 0x11

	r1, reads1 := newCounting(body)
	_, err := g.Generate(hash, r1)
	if err == nil {
		t.Fatal("setup: a truncated PNG decoded successfully, so this test has nothing to cool down")
	}
	if *reads1 == 0 {
		t.Fatal("setup: the decoder never read the body")
	}

	// The second request must be answered from the failure record, without reading the body at all.
	// Reading is the observable cost: if the cooldown works, the decoder is never entered.
	r2, reads2 := newCounting(body)
	_, err2 := g.Generate(hash, r2)
	if !errors.Is(err2, thumbnail.ErrCooldown) {
		t.Fatalf("a second request for a known-broken file returned %v, want ErrCooldown. "+
			"Without it, a corrupt or crafted file is decoded again on every request — the "+
			"semaphore and deadline bound one attempt, not the number of attempts.", err2)
	}
	if *reads2 != 0 {
		t.Fatalf("the cooldown still read the body %d time(s); the decode must not be entered again",
			*reads2)
	}
}

// TestTheCooldownIsKeyedByContentAddress checks that a replaced file is not penalised.
//
// This is the property that makes a long cooldown safe. A file at the same path with new content
// has a new address, so it is attempted immediately — otherwise "this file failed once" would
// become "this path can never have a thumbnail".
func TestTheCooldownIsKeyedByContentAddress(t *testing.T) {
	g := thumbnail.New()

	broken := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32)
	var brokenHash [32]byte
	brokenHash[0] = 0x22

	r, _ := newCounting(broken)
	if _, err := g.Generate(brokenHash, r); err == nil {
		t.Fatal("setup: the broken body decoded")
	}

	// A DIFFERENT address for the same broken bytes must still be attempted: the key is the
	// content, and a caller with a new hash has never been told this one fails.
	var otherHash [32]byte
	otherHash[0] = 0x23
	r2, reads2 := newCounting(broken)
	_, err := g.Generate(otherHash, r2)
	if errors.Is(err, thumbnail.ErrCooldown) {
		t.Fatal("a different content address was cooled down, so the record is keyed by something " +
			"other than the content — which would let one bad file block unrelated files")
	}
	if *reads2 == 0 {
		t.Fatal("a different content address was never read")
	}
}

// TestTheCooldownExpires checks that the window is a window, not a permanent verdict.
//
// A permanent record would be defensible for a pure content cache, but the failure can be
// environmental — the process ran out of memory during one decode — and a permanent verdict would
// make that file permanently thumbnail-less for the life of the process.
func TestTheCooldownExpires(t *testing.T) {
	g := thumbnail.New()
	g.SetFailureCooldownForTest(30 * time.Millisecond)

	broken := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32)
	var hash [32]byte
	hash[0] = 0x33

	r, _ := newCounting(broken)
	if _, err := g.Generate(hash, r); err == nil {
		t.Fatal("setup: the broken body decoded")
	}

	r2, _ := newCounting(broken)
	if _, err := g.Generate(hash, r2); !errors.Is(err, thumbnail.ErrCooldown) {
		t.Fatalf("immediately after a failure the address was not cooling down (%v)", err)
	}

	time.Sleep(60 * time.Millisecond)

	r3, reads3 := newCounting(broken)
	_, err := g.Generate(hash, r3)
	if errors.Is(err, thumbnail.ErrCooldown) {
		t.Fatal("the cooldown never expired, so a transient failure permanently disables the " +
			"thumbnail for that content")
	}
	if *reads3 == 0 {
		t.Fatal("after the window expired the decode was still not attempted")
	}
}
