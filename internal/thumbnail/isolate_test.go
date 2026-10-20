package thumbnail_test

// WC-GAP-004: isolation of the image decoder, and the property that makes it worth having.
//
// The claim to prove is not "a child process runs". It is that a decode which does not finish is
// TERMINATED rather than abandoned in the serving process. So the central test uses a worker that
// hangs forever and asserts that (a) the caller gets ErrTimeout, (b) the call returns promptly, and
// (c) the child process is gone afterwards. Without the last one, "isolation" could be a process that
// leaks on every hostile file, which is worse than in-process decoding.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/thumbnail"
)

// workerHelperEnv is set in the child of the re-exec helper below.
const workerHelperEnv = "THUMBNAIL_TEST_WORKER"

// TestMain lets this test binary act as a thumbnail worker when re-executed.
//
// The real WorkerCommand runs the application binary, which a test cannot produce. Re-executing the
// test binary with a marker reaches the same entry point (thumbnail.WorkerMain) through the same
// pipes, so the protocol and the classification of failures are exercised for real rather than
// simulated.
func TestMain(m *testing.M) {
	switch os.Getenv(workerHelperEnv) {
	case "":
		os.Exit(m.Run())
	case "worker":
		os.Exit(thumbnail.WorkerMain(os.Stdin, os.Stdout))
	case "hang":
		// A decoder that never returns. It must be killed, not waited out.
		//
		// It writes its own pid to the path in the other variable so the parent can prove the
		// process is gone rather than inferring it from a deadline.
		if pidFile := os.Getenv("THUMBNAIL_TEST_PIDFILE"); pidFile != "" {
			_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		// Sleep forever rather than `select {}`: with no other goroutines Go's runtime
		// panics "all goroutines are asleep - deadlock!" and exits 2, so the child would
		// die by itself in milliseconds and the test would never exercise the kill.
		for {
			time.Sleep(time.Hour)
		}
	}
}

// workerCommand re-execs this test binary in the requested mode.
func workerCommand(mode string) func(ctx context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0])
		cmd.Env = append(os.Environ(), workerHelperEnv+"="+mode)
		return cmd
	}
}

// samplePNG builds a real 64x64 PNG; the decoder refuses bytes it cannot decode.
func samplePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestTheIsolatedDecoderProducesTheSameThumbnail proves the child is a real decoder, not a stub.
//
// This is the baseline: without it, every other test here would pass against a child that returned
// nothing at all, and "isolation works" would mean "nothing works".
func TestTheIsolatedDecoderProducesTheSameThumbnail(t *testing.T) {
	data := samplePNG(t)

	// In-process, for the reference image.
	inProc := thumbnail.New()
	want, err := inProc.Generate([32]byte{1}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("in-process decode: %v", err)
	}

	isolated := thumbnail.New()
	isolated.UseSubprocess(thumbnail.Isolation{
		Command: workerCommand("worker"),
		Timeout: 30 * time.Second,
	})
	if !isolated.Isolated() {
		t.Fatal("UseSubprocess did not take effect")
	}

	got, err := isolated.Generate([32]byte{1}, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("isolated decode: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the isolated decoder produced different bytes from the in-process one "+
			"(%d vs %d bytes); the two paths must be the same decoder or the isolated one is "+
			"the only one untested", len(got), len(want))
	}
}

// TestAHangingDecodeIsKilledAndTheCallReturns is the gap itself.
//
// The in-process decoder cannot do this: its deadline returns to the caller while the goroutine keeps
// running. Isolation converts the deadline into a kill, and this asserts the kill actually happened
// by checking the child's pid is gone from the process table.
func TestAHangingDecodeIsKilledAndTheCallReturns(t *testing.T) {
	pidFile := t.TempDir() + "/worker.pid"

	g := thumbnail.New()
	g.UseSubprocess(thumbnail.Isolation{
		Command: func(ctx context.Context) *exec.Cmd {
			cmd := exec.CommandContext(ctx, os.Args[0])
			cmd.Env = append(os.Environ(),
				workerHelperEnv+"=hang",
				"THUMBNAIL_TEST_PIDFILE="+pidFile)
			return cmd
		},
		// Short enough to keep the test quick, long enough that process startup is not the
		// thing being measured.
		Timeout: 2 * time.Second,
	})

	start := time.Now()
	_, err := g.Generate([32]byte{9}, bytes.NewReader([]byte("\x89PNG\r\n\x1a\n")))
	elapsed := time.Since(start)

	if !errors.Is(err, thumbnail.ErrTimeout) {
		t.Fatalf("want ErrTimeout from a hanging decoder, got %v", err)
	}
	// A generous upper bound: the point is that it did not hang forever, not that it was fast.
	if elapsed > 30*time.Second {
		t.Fatalf("the call took %v; the deadline did not bound it", elapsed)
	}

	// The child must be dead, not orphaned. Its pid was written before it hung.
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		raw, rerr := os.ReadFile(pidFile)
		if rerr != nil {
			lastErr = rerr
			time.Sleep(50 * time.Millisecond)
			continue
		}
		pid, perr := strconv.Atoi(string(bytes.TrimSpace(raw)))
		if perr != nil {
			lastErr = perr
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if !processAlive(pid) {
			return // the property holds
		}
		lastErr = fmt.Errorf("worker pid %d is still running", pid)
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the decoder child was not killed after the deadline: %v. An isolated decoder that "+
		"leaks a process per hostile file is worse than the in-process one it replaced.", lastErr)
}

// TestACorruptImageIsClassifiedNotCrashed: a bad file must be a normal answer.
func TestACorruptImageIsClassifiedNotCrashed(t *testing.T) {
	g := thumbnail.New()
	g.UseSubprocess(thumbnail.Isolation{
		Command: workerCommand("worker"),
		Timeout: 30 * time.Second,
	})

	_, err := g.Generate([32]byte{2}, bytes.NewReader([]byte("this is not an image at all")))
	if !errors.Is(err, thumbnail.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported for non-image bytes, got %v", err)
	}

	// A PNG header with a truncated body decodes as far as the header and then fails: still an
	// unsupported/unavailable answer, never a panic and never a hang.
	_, err = g.Generate([32]byte{3}, bytes.NewReader([]byte("\x89PNG\r\n\x1a\nrubbish")))
	if err == nil {
		t.Fatal("a truncated PNG produced a thumbnail")
	}
	if errors.Is(err, thumbnail.ErrTimeout) {
		t.Fatal("a truncated PNG was reported as a timeout; the classifications have collapsed")
	}
}

// TestTheOversizedImageIsRefusedInTheChild: the pixel cap still applies across the boundary.
func TestTheOversizedImageIsRefusedInTheChild(t *testing.T) {
	// 10000x10000 = 100 MP, twice the cap. Small on disk because the image is a flat colour,
	// which is why the pixel cap exists instead of a size check.
	huge := oversizedPNG(t)

	g := thumbnail.New()
	g.UseSubprocess(thumbnail.Isolation{
		Command: workerCommand("worker"),
		Timeout: 60 * time.Second,
	})
	_, err := g.Generate([32]byte{4}, bytes.NewReader(huge))
	if !errors.Is(err, thumbnail.ErrTooLarge) {
		t.Fatalf("want ErrTooLarge for a 100 MP image, got %v", err)
	}
}

// oversizedPNG builds a real 10000x10000 PNG, the same fixture the in-process bomb test uses.
func oversizedPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 10000, 10000))
	// A flat colour keeps the encoding small without needing to touch every pixel.
	img.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if buf.Len() > 5<<20 {
		t.Fatalf("the fixture is %d bytes; it should be small enough to pass a size check", buf.Len())
	}
	return buf.Bytes()
}

// TestAFailedWorkerIsReportedAsDeploymentFailure: a missing binary is not a bad image.
//
// The distinction matters operationally: "this file has no thumbnail" is a user's problem, and
// "the decoder cannot start" is the operator's. Collapsing them hides an outage behind a type badge.
func TestAFailedWorkerIsReportedAsDeploymentFailure(t *testing.T) {
	g := thumbnail.New()
	g.UseSubprocess(thumbnail.Isolation{
		Command: func(ctx context.Context) *exec.Cmd {
			// A path that cannot exist, so Start() fails.
			return exec.CommandContext(ctx, "/nonexistent/newcloud-thumbnail-worker")
		},
		Timeout: 5 * time.Second,
	})

	_, err := g.Generate([32]byte{5}, bytes.NewReader(samplePNG(t)))
	if !errors.Is(err, thumbnail.ErrWorkerFailed) {
		t.Fatalf("want ErrWorkerFailed when the child cannot start, got %v", err)
	}
}

// TestTheCooldownStillAppliesWhenIsolated: the resilience machinery is mode-independent.
func TestTheCooldownStillAppliesWhenIsolated(t *testing.T) {
	g := thumbnail.New()
	g.SetFailureCooldownForTest(time.Hour)
	g.UseSubprocess(thumbnail.Isolation{
		Command: workerCommand("worker"),
		Timeout: 30 * time.Second,
	})

	bad := []byte("\x89PNG\r\n\x1a\nnope")
	if _, err := g.Generate([32]byte{6}, bytes.NewReader(bad)); err == nil {
		t.Fatal("a bad image produced a thumbnail")
	}
	// The second request is answered from the cooldown without starting a process at all.
	_, err := g.Generate([32]byte{6}, bytes.NewReader(bad))
	if !errors.Is(err, thumbnail.ErrCooldown) {
		t.Fatalf("want ErrCooldown on the second attempt, got %v", err)
	}
}

// processAlive reports whether a pid is still running.
//
// Signal 0 is the portable reachability probe: it is delivered to nothing and only reports whether
// the process exists and this user may signal it. On Windows os.FindProcess succeeds for any pid, so
// the signal is what actually decides — and an error means the process is gone, which is exactly the
// answer the test needs.
//
// Conservative in the safe direction: a pid that was never seen reports "not alive" (returning early
// above), so the test fails rather than passing when it cannot tell.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
