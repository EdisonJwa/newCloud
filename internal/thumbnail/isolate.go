package thumbnail

// Process isolation for image decoding (WC-GAP-004, §6.3, T-14).
//
// The gap this closes is narrow and real. The in-process decoder already has a pixel cap, a
// concurrency semaphore and a wall-clock deadline, and none of those is a sandbox: a Go decoder
// goroutine CANNOT BE KILLED. When `Generate` returns ErrTimeout it stops waiting, but the goroutine
// keeps decoding, keeps holding its memory, and (because it is CPU-bound) keeps competing with the
// server that is meant to be serving files. A file crafted to make the decoder spin therefore buys an
// attacker real work in the request-serving process, and repeating it stacks more of it.
//
// Isolation fixes the property the deadline could only approximate. The decode moves into a child
// process; the deadline becomes a kill; memory and CPU spent on a hostile file die with that process
// instead of accumulating in the parent. The parent keeps its pixel cap and semaphore — they bound
// how much work is requested at all, which a subprocess does not help with.
//
// Design notes that are load-bearing:
//
//   - It is the SAME binary, re-executed with an environment marker, not a second program. The
//     project's promise is one static binary; shipping a companion decoder would break that and
//     would not survive a `go install`.
//   - The child speaks a deliberately tiny protocol: image bytes on stdin, PNG on stdout, a reason
//     string on stderr, and an exit code. Nothing else crosses the boundary, and in particular no
//     credential, database handle or configuration.
//   - The child gets a MINIMAL environment. Inheriting the parent's would hand a decoding bug the
//     database URL and the session secret for free.
//   - The output is read through a limit. A parent that trusted the child's stdout could be made to
//     allocate whatever the child emits, which would move the bomb rather than stop it.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// workerEnv marks a re-executed process as the thumbnail worker.
//
// The value is the pixel cap, so the child enforces exactly the limit its parent was built with
// instead of a compiled-in default that could drift between them.
const workerEnv = "NEWCLOUD_THUMBNAIL_WORKER"

// Exit codes the worker uses, so the parent can tell "this is not an image" from "the decoder died".
//
// Deliberately in a high, distinctive range. A Go panic exits with 2 and a shell reports 127 for a
// missing command, so codes 1-3 are exactly the values a crashing child produces by accident —
// mapping them onto the decoder's own meanings would report a crashed worker as "not an image" and
// hide a broken deployment behind a type badge. Any code outside this set is unconditionally
// ErrWorkerFailed.
const (
	workerOK          = 0
	workerUnsupported = 10
	workerTooLarge    = 11
	workerFailed      = 12
	workerHung        = 13
)

// maxWorkerInput bounds how many image bytes are written to a child.
//
// 128 MiB is far above any file that can decode within the 50 MP cap — a 50 MP PNG with 16-bit
// channels is around 150 MB uncompressed but compresses to a few tens of MB — and far below the
// point where streaming it into a pipe would itself be the attack. A larger file is refused as
// ErrTooLarge rather than streamed, because refusing costs the user a type badge and streaming costs
// the server its memory.
const maxWorkerInput = 128 << 20

// maxWorkerOutput bounds the thumbnail the parent will accept back.
//
// A 320-pixel PNG is a few hundred KB at worst. Four MiB leaves room for a pathological but legitimate
// worst case while making it impossible for the child to hand the parent an allocation it did not
// choose.
const maxWorkerOutput = 4 << 20

// ErrWorkerFailed means the isolated decoder could not be run or did not answer.
//
// Distinct from the decode errors above so the request path can log a deployment fault (a missing
// executable, a full process table) separately from a bad file, which is the difference between
// "this image is broken" and "thumbnails are down".
var ErrWorkerFailed = errors.New("thumbnail: isolated decoder unavailable")

// Isolation configures the generator to decode in a child process.
type Isolation struct {
	// Command builds the child. It must produce a process that runs the worker entry point; the
	// default is the current executable re-run with workerEnv set.
	Command func(ctx context.Context) *exec.Cmd
	// Timeout bounds the child's wall clock. Zero uses DefaultTimeout.
	Timeout time.Duration
}

// WorkerCommand returns the default child command: this executable, re-run in worker mode.
//
// os.Executable rather than os.Args[0]: the latter is whatever the operator typed, which may be a
// relative path that does not resolve from the process's eventual working directory.
//
// In a test binary os.Executable is the test binary, which does not run main and so has no worker
// entry point. tests replace this via Isolation.Command; a production binary always resolves to
// itself.
func WorkerCommand(ctx context.Context, maxPixelsLimit int) *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		// A caller that cannot resolve its own binary cannot isolate. Returning a command that
		// fails to start is better than panicking: the caller reports ErrWorkerFailed and the
		// request degrades to "no thumbnail" instead of taking the server down.
		exe = os.Args[0]
	}
	cmd := exec.CommandContext(ctx, exe)
	// The child is not a server and must not inherit the parent's credentials. PATH and the
	// Windows system variables are all it needs to start; the environment is rebuilt rather than
	// filtered so a new secret in the parent does not silently join the child.
	cmd.Env = workerEnvFor(maxPixelsLimit)
	return cmd
}

// workerEnvFor builds the child's environment.
func workerEnvFor(maxPixelsLimit int) []string {
	env := []string{workerEnv + "=" + strconv.Itoa(maxPixelsLimit)}
	for _, k := range []string{"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "TMPDIR", "TMP", "TEMP", "PATH"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// UseSubprocess switches g to decoding in a child process.
//
// Called once at startup. It is not safe to toggle while requests are in flight, and there is no
// reason to: the deployment either can fork or it cannot.
func (g *Generator) UseSubprocess(iso Isolation) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if iso.Command == nil {
		iso.Command = func(ctx context.Context) *exec.Cmd {
			return WorkerCommand(ctx, maxPixels)
		}
	}
	if iso.Timeout <= 0 {
		iso.Timeout = DefaultTimeout
	}
	g.iso = &iso
}

// Isolated reports whether decoding happens in a child process.
func (g *Generator) Isolated() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.iso != nil
}

// decode runs the decoder, in a child process when isolation is configured.
//
// This is the single place the two modes diverge, so the parent's cache, cooldown, semaphore and
// failure accounting are identical either way — an isolated deployment must not behave differently
// under load, only survive a hostile file differently.
func (g *Generator) decode(r io.ReadSeeker) ([]byte, error) {
	g.mu.RLock()
	iso := g.iso
	g.mu.RUnlock()
	if iso == nil {
		return renderPNG(r)
	}
	return g.decodeIsolated(iso, r)
}

// decodeIsolated runs one decode in a child process and kills it if it overruns.
func (g *Generator) decodeIsolated(iso *Isolation, r io.ReadSeeker) ([]byte, error) {
	// A fresh context per decode: reusing one would let the first timeout cancel every later
	// request, which is a self-inflicted outage from a single bad file.
	ctx, cancel := context.WithTimeout(context.Background(), iso.Timeout)
	defer cancel()

	cmd := iso.Command(ctx)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: stdin: %v", ErrWorkerFailed, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: stdout: %v", ErrWorkerFailed, err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// WaitDelay is what makes the kill unconditional. Killing the child closes its end of the
	// pipes, but a grandchild (or a child that forked) can keep them open, and cmd.Wait would
	// then block past the deadline. With WaitDelay set, Wait returns anyway and closes the pipes.
	cmd.WaitDelay = 2 * time.Second

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: start: %v", ErrWorkerFailed, err)
	}

	// Feed the child and collect its output concurrently. Doing either in sequence deadlocks as
	// soon as a real image exceeds a pipe buffer: the child blocks writing its PNG while the
	// parent blocks writing the rest of the input.
	var (
		writeErr  error
		copyBytes int64
		wg        sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer stdin.Close()
		n, cerr := io.Copy(stdin, io.LimitReader(r, maxWorkerInput+1))
		copyBytes = n
		writeErr = cerr
	}()

	out, readErr := io.ReadAll(io.LimitReader(stdout, maxWorkerOutput+1))
	wg.Wait()

	waitErr := cmd.Wait()

	// The deadline is checked first: a killed child produces an exit error AND a truncated
	// output, and reporting the truncation would hide the cause.
	if ctx.Err() == context.DeadlineExceeded {
		return nil, ErrTimeout
	}

	if copyBytes > maxWorkerInput {
		// The input was larger than any image this package will consider. The child saw a
		// truncated stream and failed, which is why this is checked before its exit code.
		return nil, ErrTooLarge
	}
	if writeErr != nil && !errors.Is(writeErr, io.ErrClosedPipe) {
		return nil, fmt.Errorf("%w: write input: %v", ErrWorkerFailed, writeErr)
	}
	if readErr != nil && len(out) == 0 {
		return nil, fmt.Errorf("%w: read output: %v", ErrWorkerFailed, readErr)
	}
	if len(out) > maxWorkerOutput {
		return nil, fmt.Errorf("%w: the decoder returned %d bytes, above the %d limit",
			ErrWorkerFailed, len(out), maxWorkerOutput)
	}

	if waitErr != nil {
		// The child classifies its own failures, because it is the side that knows why.
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			switch exitErr.ExitCode() {
			case workerUnsupported:
				return nil, ErrUnsupported
			case workerTooLarge:
				return nil, ErrTooLarge
			case workerHung:
				return nil, ErrTimeout
			}
		}
		msg := stderr.String()
		if msg == "" {
			return nil, fmt.Errorf("%w: %v", ErrWorkerFailed, waitErr)
		}
		return nil, fmt.Errorf("%w: %s", ErrWorkerFailed, firstLine(msg))
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("%w: the decoder produced no output", ErrWorkerFailed)
	}
	return out, nil
}

// firstLine keeps a multi-line decoder traceback out of a single log line.
func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return s[:i]
		}
	}
	return s
}

// WorkerMain runs the isolated decoder and is the process's whole job.
//
// It is called from main before any configuration is loaded, so a worker process never opens a
// database connection, a listener or a log file. Returns the exit code.
//
// The pixel cap comes from the environment so the child enforces the parent's limit verbatim; a
// compiled-in constant could drift from the parent's if only one of them were changed.
func WorkerMain(stdin io.Reader, stdout io.Writer) int {
	limit := maxPixels
	if v := os.Getenv(workerEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	// Bound what the child will read. The parent also bounds what it writes; both sides bound
	// because a compromised or confused peer must not be able to make this process allocate.
	data, err := io.ReadAll(io.LimitReader(stdin, maxWorkerInput+1))
	if err != nil {
		fmt.Fprintln(os.Stderr, "read input:", err)
		return workerFailed
	}
	if len(data) > maxWorkerInput {
		fmt.Fprintln(os.Stderr, "input above the size limit")
		return workerTooLarge
	}

	r := bytes.NewReader(data)

	// Fail fast on a non-image and on an oversized one: these are the cheap, common answers and
	// the parent maps them to a type badge rather than an error.
	head := make([]byte, 64)
	n, _ := io.ReadFull(r, head)
	if _, ok := Kind(head[:n]); !ok {
		return workerUnsupported
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		fmt.Fprintln(os.Stderr, "seek:", err)
		return workerFailed
	}
	cfg, err := imageConfig(r)
	if err != nil {
		// A header that cannot be read is not a usable image. Classified as unsupported rather
		// than failed so a corrupt upload does not look like a broken deployment.
		return workerUnsupported
	}
	if cfg.w*cfg.h > limit {
		return workerTooLarge
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		fmt.Fprintln(os.Stderr, "seek:", err)
		return workerFailed
	}

	png, err := renderPNG(r)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnsupported):
			return workerUnsupported
		case errors.Is(err, ErrTooLarge):
			return workerTooLarge
		}
		// The decode error itself is deliberately NOT echoed to the parent's log verbatim: it
		// describes the attacker's file, and the parent records the content address instead.
		fmt.Fprintln(os.Stderr, "decode failed")
		return workerFailed
	}

	if _, err := stdout.Write(png); err != nil {
		return workerFailed
	}
	return workerOK
}

// IsWorkerProcess reports whether this process was started as a thumbnail worker.
//
// main calls this before anything else so a worker never parses configuration or opens a database.
func IsWorkerProcess() bool { return os.Getenv(workerEnv) != "" }

// dims is the tiny shape the worker needs, so it does not carry an image.Config around.
type dims struct{ w, h int }

// imageConfig reads the dimensions without allocating the bitmap.
//
// The same call renderPNG makes, so the worker's refusal and the decoder's refusal agree. Two
// different size checks would let a file pass one and be refused by the other, which reads to the
// client as an intermittent failure.
func imageConfig(r io.Reader) (dims, error) {
	cfg, _, err := image.DecodeConfig(r)
	if err != nil {
		return dims{}, err
	}
	return dims{w: cfg.Width, h: cfg.Height}, nil
}

// DefaultWorkerConcurrency is how many isolated decoders may run at once.
//
// The parent's semaphore already bounds this; the constant exists so the two limits are stated in
// one place and a reader does not have to infer the coupling.
const DefaultWorkerConcurrency = DefaultConcurrency
