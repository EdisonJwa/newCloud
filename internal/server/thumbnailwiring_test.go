package server_test

// WC-GAP-004 at the wiring layer.
//
// internal/thumbnail proves the isolated decoder works, and every one of those tests would still
// pass if internal/server stopped calling UseSubprocess. That would leave `limits.thumbnail_isolation`
// validated, documented, defaulting to "process", and read by nothing that builds the decoder — the
// "implemented and not wired" defect this codebase keeps finding. So the wiring gets its own test.
//
// The harness disables isolation by default (a test binary cannot serve as its own worker, since
// WorkerCommand re-execs os.Executable), so these assertions construct a server explicitly.

import (
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/server"
)

// TestTheServerIsolatesThumbnailsByDefault pins the production default.
func TestTheServerIsolatesThumbnailsByDefault(t *testing.T) {
	h := newHarnessTuned(t, func(c *config.Config) {
		// Restore the production default that the harness overrides.
		c.Limits.ThumbnailIsolation = config.ThumbnailIsolationProcess
	}, nil)

	if !h.srvServer.ThumbnailsAreIsolatedForTest() {
		t.Fatal("the server built an in-process decoder while thumbnail_isolation was \"process\". " +
			"A crafted image would keep burning CPU in the request-serving process after its " +
			"deadline returned to the client, which is the whole gap WC-GAP-004 closes.")
	}
}

// TestInlineChoosesTheInProcessDecoder: the opt-out must actually take effect.
//
// Without this half, a wiring bug that always isolated would pass the test above and make the
// documented escape hatch a lie.
func TestInlineChoosesTheInProcessDecoder(t *testing.T) {
	h := newHarnessTuned(t, func(c *config.Config) {
		c.Limits.ThumbnailIsolation = config.ThumbnailIsolationInline
	}, nil)

	if h.srvServer.ThumbnailsAreIsolatedForTest() {
		t.Fatal("thumbnail_isolation: inline still built an isolating decoder, so an operator who " +
			"needs in-process decoding (no fork, restricted container) has no working option")
	}
}

// TestAnEmptyValueIsolates: a missing key must get the safe behaviour.
//
// The config validator accepts "" as well as an explicit value, and ThumbnailIsolated treats
// anything other than "inline" as isolate. That ordering is deliberate — a typo should not silently
// restore the weaker decoder — and this pins it at the server, where the consequence lives.
func TestAnEmptyValueIsolates(t *testing.T) {
	h := newHarnessTuned(t, func(c *config.Config) {
		c.Limits.ThumbnailIsolation = ""
	}, nil)

	if !h.srvServer.ThumbnailsAreIsolatedForTest() {
		t.Fatal("an empty thumbnail_isolation built an in-process decoder; a missing key must not " +
			"be the weaker choice")
	}
}

var _ = server.Deps{}
