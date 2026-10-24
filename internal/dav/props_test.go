package dav

// PROPFIND property coverage on both layouts (2026-10-24 review, M3).
//
// Three things were missing and they compounded. supportedlock and lockdiscovery are class-2
// MUSTs — without them a client cannot tell "not locked" from "this server has no locks", and
// Windows/Office fall back to their least informative error. Dead properties were persisted by
// PROPPATCH and never returned by PROPFIND, so every client that stored per-file metadata found
// it gone on the next read and concluded the server had ignored the write. And a property the
// server did not have was simply omitted, which RFC 4918 §9.1 forbids: the client cannot tell
// "not set" from "ignored".

import (
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

const propfindAllProp = `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`

// A class-2 server advertises supportedlock and reports an empty lockdiscovery on a resource
// nobody holds a lock on. The empty element is the point: its absence is what a client reads
// as "locks are not supported here".
func TestPropfindAdvertisesLockSupport(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("plain.txt"), strings.NewReader("x"), nil)

	rec := e.do(t, "PROPFIND", e.davPath("plain.txt"),
		strings.NewReader(propfindAllProp), map[string]string{"Depth": "0"})
	body := rec.Body.String()
	if !strings.Contains(body, "<D:supportedlock>") {
		t.Fatalf("PROPFIND did not advertise supportedlock:\n%s", body)
	}
	if !strings.Contains(body, "<D:lockentry>") || !strings.Contains(body, "<D:exclusive/>") {
		t.Fatalf("supportedlock must name the exclusive write scope:\n%s", body)
	}
	if !strings.Contains(body, "<D:lockdiscovery></D:lockdiscovery>") {
		t.Fatalf("an unlocked resource must report an EMPTY lockdiscovery, not omit it:\n%s", body)
	}
}

// A held lock must be visible through PROPFIND, token and all: that is how a client shows
// "locked by X" and how a second client discovers why its write was refused.
func TestPropfindReportsTheActiveLock(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("busy.txt"), strings.NewReader("x"), nil)
	token := lockPath(t, e, "busy.txt", "0")

	rec := e.do(t, "PROPFIND", e.davPath("busy.txt"),
		strings.NewReader(propfindAllProp), map[string]string{"Depth": "0"})
	body := rec.Body.String()
	if !strings.Contains(body, token) {
		t.Fatalf("lockdiscovery did not name the held lock %q:\n%s", token, body)
	}
	if !strings.Contains(body, "<D:lockdiscovery><D:activelock>") {
		t.Fatalf("lockdiscovery must contain an activelock:\n%s", body)
	}
	if !strings.Contains(body, "<D:depth>0</D:depth>") {
		t.Fatalf("the lock was taken with Depth: 0 and must report it:\n%s", body)
	}
}

// A dead property written by PROPPATCH must come back in PROPFIND. It is what makes PROPPATCH
// mean anything to a client that sets metadata and re-reads it.
func TestPropfindReturnsDeadProperties(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, _ layout.Mode) {
		e.do(t, http.MethodPut, e.davPath("meta.txt"), strings.NewReader("x"), nil)

		patch := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:example:">
			<D:set><D:prop><Z:colour>blue</Z:colour><Z:rank>3</Z:rank></D:prop></D:set>
			</D:propertyupdate>`
		if rec := e.do(t, "PROPPATCH", e.davPath("meta.txt"), strings.NewReader(patch), nil); rec.Code != http.StatusMultiStatus {
			t.Fatalf("PROPPATCH: want 207, got %d", rec.Code)
		}

		rec := e.do(t, "PROPFIND", e.davPath("meta.txt"),
			strings.NewReader(propfindAllProp), map[string]string{"Depth": "0"})
		body := rec.Body.String()
		for _, want := range []string{"colour", "blue", "rank", "3"} {
			if !strings.Contains(body, want) {
				t.Fatalf("PROPFIND did not return the dead property value %q:\n%s", want, body)
			}
		}
	})

	// The named form must work too, because that is how Office asks.
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("named.txt"), strings.NewReader("x"), nil)
	patch := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:example:z">
		<D:set><D:prop><Z:flag>on</Z:flag></D:prop></D:set></D:propertyupdate>`
	if rec := e.do(t, "PROPPATCH", e.davPath("named.txt"), strings.NewReader(patch), nil); rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPPATCH: %d", rec.Code)
	}
	ask := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:" xmlns:Z="urn:example:z">
		<D:prop><Z:flag/></D:prop></D:propfind>`
	body := e.do(t, "PROPFIND", e.davPath("named.txt"),
		strings.NewReader(ask), map[string]string{"Depth": "0"}).Body.String()
	if !strings.Contains(body, "on") {
		t.Fatalf("a named dead property was not returned:\n%s", body)
	}
}

// A property the server does not have must be answered in a 404 propstat. Omitting it left the
// client unable to distinguish "not set" from "the server ignored that part of the request".
func TestPropfindAnswersUnknownPropertiesAsNotFound(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("props.txt"), strings.NewReader("x"), nil)

	ask := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:" xmlns:Z="urn:example:missing">
		<D:prop>
			<D:getetag/>
			<D:getcontentlanguage/>
			<Z:never-set/>
		</D:prop></D:propfind>`
	rec := e.do(t, "PROPFIND", e.davPath("props.txt"),
		strings.NewReader(ask), map[string]string{"Depth": "0"})
	body := rec.Body.String()

	if !strings.Contains(body, "HTTP/1.1 200 OK") {
		t.Fatalf("the properties that DO exist must be in a 200 propstat:\n%s", body)
	}
	if !strings.Contains(body, "HTTP/1.1 404 Not Found") {
		t.Fatalf("a missing property must be answered in a 404 propstat, not omitted:\n%s", body)
	}
	if !strings.Contains(body, "getcontentlanguage") {
		t.Fatalf("the missing DAV property must be NAMED in the 404 propstat:\n%s", body)
	}
	if !strings.Contains(body, "never-set") {
		t.Fatalf("the missing dead property must be NAMED in the 404 propstat:\n%s", body)
	}
	// The property that does exist must not be in the 404 bucket.
	if strings.Contains(body, "<D:getetag/>") {
		t.Fatalf("an existing property was reported as missing:\n%s", body)
	}
}

// A LOCK reports the depth the client asked for. It used to print "infinity" always, so a
// client that locked a collection with Depth: 0 believed it had locked the whole subtree.
func TestLockResponseReportsTheDepthAskedFor(t *testing.T) {
	e := newEnv(t)
	e.mustMkcol(t, "locked-collection")

	rec := e.do(t, "LOCK", e.davPath("locked-collection"),
		strings.NewReader(fenceLockBody), map[string]string{"Depth": "0"})
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK: want 200, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<D:depth>0</D:depth>") {
		t.Fatalf("LOCK with Depth: 0 must answer <D:depth>0</D:depth>:\n%s", body)
	}

	// The default is infinity, and must still be reported as such.
	e.do(t, http.MethodPut, e.davPath("inf.txt"), strings.NewReader("x"), nil)
	rec = e.do(t, "LOCK", e.davPath("inf.txt"),
		strings.NewReader(fenceLockBody), map[string]string{"Depth": "infinity"})
	if body := rec.Body.String(); !strings.Contains(body, "<D:depth>infinity</D:depth>") {
		t.Fatalf("LOCK with Depth: infinity must answer infinity:\n%s", body)
	}
}
