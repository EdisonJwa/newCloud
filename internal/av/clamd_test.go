package av

// Protocol tests against a real TCP listener speaking the INSTREAM protocol, because the
// protocol framing — the z-command, the big-endian length prefix, the zero terminator — is
// exactly the part a fake interface cannot check. The listener is clamd's half of the
// conversation, not a stub of this package's own interface.

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeClamd accepts one session, reads the stream to the terminator, and answers with the
// canned verdict line.
type fakeClamd struct {
	listen net.Listener
	answer string
	got    strings.Builder
}

func newFakeClamd(t *testing.T, answer string) *fakeClamd {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeClamd{listen: l, answer: answer}
	go f.serve()
	t.Cleanup(func() { l.Close() })
	return f
}

func (f *fakeClamd) serve() {
	conn, err := f.listen.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Read the greeting: "zINSTREAM\0".
	head := make([]byte, 10)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	// Chunks until the zero-length terminator, echoing the bytes for the test's assertions.
	for {
		var n uint32
		if err := binary.Read(conn, binary.BigEndian, &n); err != nil {
			return
		}
		if n == 0 {
			_, _ = conn.Write([]byte(f.answer))
			return
		}
		if _, err := io.CopyN(&f.got, conn, int64(n)); err != nil {
			return
		}
	}
}

func (f *fakeClamd) addr() string { return f.listen.Addr().String() }

func TestClamdStreamsTheWholeInputAndParsesClean(t *testing.T) {
	f := newFakeClamd(t, "stream: OK\n")
	c := NewClamd(f.addr(), 2*time.Second)

	verdict, sig, err := c.Scan(context.Background(), strings.NewReader(strings.Repeat("a", 100_000)))
	if err != nil {
		t.Fatal(err)
	}
	if verdict != Clean || sig != "" {
		t.Fatalf("verdict=%q sig=%q, want clean", verdict, sig)
	}
	// 100 000 bytes must arrive complete: a client that drops the tail of the final chunk
	// would scan a different file than the one the user uploaded.
	if f.got.Len() != 100_000 {
		t.Fatalf("daemon received %d bytes, want 100000", f.got.Len())
	}
}

func TestClamdParsesTheSignature(t *testing.T) {
	f := newFakeClamd(t, "stream: Win.Test.EICAR.HSTR-1 FOUND\n")
	c := NewClamd(f.addr(), 2*time.Second)

	verdict, sig, err := c.Scan(context.Background(), strings.NewReader("X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR"))
	if err != nil {
		t.Fatal(err)
	}
	if verdict != Infected || sig != "Win.Test.EICAR.HSTR-1" {
		t.Fatalf("verdict=%q sig=%q, want infected with the signature", verdict, sig)
	}
}

func TestClamdRefusesToGuessAnUnknownAnswer(t *testing.T) {
	f := newFakeClamd(t, "INSTREAM size limit exceeded\n")
	c := NewClamd(f.addr(), 2*time.Second)

	_, _, err := c.Scan(context.Background(), strings.NewReader("bytes"))
	if err == nil || !strings.Contains(err.Error(), "scanner unavailable") {
		t.Fatalf("a daemon refusal became %v; it must be an error, never a clean verdict", err)
	}
}

func TestClamdReportsADownDaemonAsUnavailable(t *testing.T) {
	// Port 1 on loopback: nothing legitimate listens there.
	c := NewClamd("127.0.0.1:1", 500*time.Millisecond)
	_, _, err := c.Scan(context.Background(), strings.NewReader("bytes"))
	if err == nil || !strings.Contains(err.Error(), "scanner unavailable") {
		t.Fatalf("a down daemon became %v", err)
	}
}

func TestParseVerdictToleratesLineEndings(t *testing.T) {
	v, _, err := parseVerdict("stream: OK\r\n\x00")
	if err != nil || v != Clean {
		t.Fatalf("CRLF+NUL answer: %v %v", v, err)
	}
}
