// Package av scans content for malware through a ClamAV clamd daemon.
//
// The INSTREAM protocol is the whole integration: open a TCP connection, say "zINSTREAM",
// stream length-prefixed chunks, end with a zero-length chunk, read one line — "stream: OK" or
// "stream: <SIGNATURE> FOUND". No library, no daemon management: a self-hosted deployment
// already has clamd or it does not, and this package talks to it when it does.
//
// The verdict is a value, not an action. What a detection means — quarantine, notify, audit —
// is the worker's decision, because that is where the transaction that makes those three
// changes atomically lives.
package av

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Verdict is the outcome of one scan.
type Verdict string

const (
	// Clean means the scanner saw the whole stream and found nothing.
	Clean Verdict = "clean"
	// Infected carries the signature the scanner named.
	Infected Verdict = "infected"
)

// ErrScannerUnavailable wraps every transport failure: a daemon that is down, a
// connection that dropped mid-stream, a response that is not the protocol. Callers treat it as
// "could not scan", which is a different decision from "scanned and clean" — conflating the
// two is how a broken daemon silently becomes an allow-all.
var ErrScannerUnavailable = errors.New("av: scanner unavailable")

// Scanner scans one stream. The worker's job handler is written against this interface, so a
// test speaks the verdicts without a daemon.
type Scanner interface {
	Scan(ctx context.Context, r io.Reader) (Verdict, string, error)
}

// Clamd is a clamd client over TCP.
type Clamd struct {
	addr    string
	timeout time.Duration
}

// NewClamd builds a client for host:port (default 127.0.0.1:3310 — the loopback daemon a
// self-hosted deployment runs beside the binary).
func NewClamd(addr string, timeout time.Duration) *Clamd {
	if addr == "" {
		addr = "127.0.0.1:3310"
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Clamd{addr: addr, timeout: timeout}
}

// chunkSize is the INSTREAM block. 32 KiB is clamd's documented maximum; larger must be
// split by the client, so this is a protocol constant rather than a tuning knob.
const chunkSize = 32 * 1024

// Scan streams r to the daemon in one session and returns the verdict plus the signature
// when the verdict is Infected.
func (c *Clamd) Scan(ctx context.Context, r io.Reader) (Verdict, string, error) {
	d := &net.Dialer{Timeout: c.timeout}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return "", "", fmt.Errorf("%w: dial %s: %v", ErrScannerUnavailable, c.addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(c.timeout))

	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return "", "", fmt.Errorf("%w: greeting: %v", ErrScannerUnavailable, err)
	}

	buf := make([]byte, chunkSize)
	header := make([]byte, 4)
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			binary.BigEndian.PutUint32(header, uint32(n))
			if _, err := conn.Write(header); err != nil {
				return "", "", fmt.Errorf("%w: chunk header: %v", ErrScannerUnavailable, err)
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return "", "", fmt.Errorf("%w: chunk body: %v", ErrScannerUnavailable, err)
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			// ErrUnexpectedEOF is the FINAL short chunk: ReadFull still delivered the n
			// bytes above, so the partial tail has been sent and the stream is done.
			break
		}
		if rerr != nil {
			return "", "", fmt.Errorf("%w: read input: %v", ErrScannerUnavailable, rerr)
		}
	}
	// The zero-length chunk ends the stream; the daemon answers once, on one line.
	binary.BigEndian.PutUint32(header, 0)
	if _, err := conn.Write(header); err != nil {
		return "", "", fmt.Errorf("%w: end marker: %v", ErrScannerUnavailable, err)
	}

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && line == "" {
		return "", "", fmt.Errorf("%w: read verdict: %v", ErrScannerUnavailable, err)
	}
	return parseVerdict(line)
}

// parseVerdict maps clamd's one-line answers onto the two-value verdict. Anything the daemon
// says that is not one of the two known answers is an error, not a guess: "INSTREAM size limit
// exceeded" means the scan did not happen, and treating it as clean would be the allow-all
// this package exists to avoid.
func parseVerdict(line string) (Verdict, string, error) {
	s := trimSpace(line)
	switch {
	case s == "stream: OK":
		return Clean, "", nil
	case hasSuffix(s, " FOUND"):
		sig := s[len("stream: "):]
		sig = trimSpace(sig[:len(sig)-len(" FOUND")])
		return Infected, sig, nil
	default:
		return "", "", fmt.Errorf("%w: daemon said %q", ErrScannerUnavailable, s)
	}
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == 0) {
		s = s[:len(s)-1]
	}
	for len(s) > 0 && (s[0] == ' ' || s[0] == 0) {
		s = s[1:]
	}
	return s
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
