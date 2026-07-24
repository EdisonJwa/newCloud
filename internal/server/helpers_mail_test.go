package server_test

// Test helpers for the email-share and audit-export tests.
//
// They live here rather than in the main harness because they exist for one purpose:
// observing something that is normally out of band. A verification code never appears in
// an HTTP response — that is the point of sending it by mail — so a test needs a seam
// where the code can be read. That seam is a fake mail transport, not a debug endpoint.

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"sync"
	"testing"
)

// recordingMailer captures messages instead of sending them.
type recordingMailer struct {
	mu   sync.Mutex
	msgs []capturedMail
}

type capturedMail struct {
	To    string
	Label string
	Code  string
	TTL   string
}

func (m *recordingMailer) SendRecipientCode(to, shareLabel, code, ttl string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, capturedMail{To: to, Label: shareLabel, Code: code, TTL: ttl})
}

// codeFor returns the most recent code sent to an address.
func (m *recordingMailer) codeFor(address string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.msgs) - 1; i >= 0; i-- {
		if m.msgs[i].To == address {
			return m.msgs[i].Code
		}
	}
	return ""
}

// lastRecipientCode returns the code the server generated for an address.
//
// The harness wires a recordingMailer, so no network is involved and no real message
// leaves the process.
func (h *harness) lastRecipientCode(t *testing.T, address string) string {
	t.Helper()
	if h.mailer == nil {
		t.Fatal("harness has no mail recorder")
	}
	return h.mailer.codeFor(address)
}

// client returns an HTTP client with its own cookie jar, for the anonymous share flows.
//
// A separate jar is what makes "a visitor with no session" testable, and it is also how
// the share cookies behave: they are scoped to the share path and must not be confused
// with a login session.
func (h *harness) client(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

// multipartBody builds a multipart body for an upload.
func multipartBody(t *testing.T, dir, name, content string) *multipartBuffer {
	t.Helper()

	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	if err := mw.WriteField("path", dir); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fw, content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &multipartBuffer{Buffer: buf, boundary: mw.Boundary()}
}

// multipartBuffer carries the boundary alongside the body.
type multipartBuffer struct {
	*bytes.Buffer
	boundary string
}

// Boundary returns the multipart boundary for the Content-Type header.
func (m *multipartBuffer) Boundary() string { return m.boundary }

// jsonBody encodes a value as a request body.
func jsonBody(t *testing.T, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}
