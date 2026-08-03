package auth_test

// ID-07 rules: what happens while the identity provider is unreachable.
//
// These test the holder rather than a live server, because the properties are about state
// transitions: no provider, discovery failing, discovery succeeding later, and the rate limit
// that keeps a login-page retry loop from hammering an IdP that is already struggling.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/auth"
)

// discoverFunc is a controllable discovery implementation.
func discoverFunc(calls *atomic.Int32, err error, p *auth.Provider) func(context.Context) (*auth.Provider, error) {
	return func(context.Context) (*auth.Provider, error) {
		calls.Add(1)
		return p, err
	}
}

type issuer struct{}

func (issuer) IssuerURL() string { return "http://idp.test" }

// TestHolderStartsEmpty: the server is allowed to run with no provider (ID-07 rule 1).
func TestHolderStartsEmpty(t *testing.T) {
	h := auth.NewProviderHolder()
	if _, ok := h.Get(); ok {
		t.Fatal("a fresh holder must report no provider")
	}
	if h.LastError() != nil {
		t.Fatalf("a fresh holder has no last error, got %v", h.LastError())
	}
}

// TestResolveReturnsErrorWhileIdPIsDown: discovery failure must surface, not be swallowed.
func TestResolveReturnsErrorWhileIdPIsDown(t *testing.T) {
	h := auth.NewProviderHolder()
	var calls atomic.Int32
	upstream := errors.New("connection refused")

	_, err := h.Resolve(context.Background(), issuer{}, discoverFunc(&calls, upstream, nil), time.Nanosecond)
	if err == nil {
		t.Fatal("Resolve must report the failure")
	}
	if !errors.Is(err, upstream) {
		t.Fatalf("the underlying cause must be preserved, got %v", err)
	}
	if h.LastError() == nil {
		t.Fatal("the holder must remember the failure so readiness and the login error can report it")
	}
	if _, ok := h.Get(); ok {
		t.Fatal("a failed discovery must not leave a provider behind")
	}
}

// TestResolveRetriesAreRateLimited is rule 5's operational half: a login page hit in a loop
// during an outage must not produce one discovery request per page load.
func TestResolveRetriesAreRateLimited(t *testing.T) {
	h := auth.NewProviderHolder()
	var calls atomic.Int32
	upstream := errors.New("still down")

	// First attempt actually runs discovery.
	if _, err := h.Resolve(context.Background(), issuer{}, discoverFunc(&calls, upstream, nil), time.Hour); err == nil {
		t.Fatal("want an error")
	}
	if calls.Load() != 1 {
		t.Fatalf("first call should have run discovery once, ran %d", calls.Load())
	}

	// The next several calls are inside the interval: no further discovery.
	for i := 0; i < 5; i++ {
		_, _ = h.Resolve(context.Background(), issuer{}, discoverFunc(&calls, upstream, nil), time.Hour)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls inside the retry interval must not re-run discovery, ran %d", calls.Load())
	}
}

// TestResolveSucceedsWhenTheIdPComesBack: recovery must not need a restart, which is the whole
// point — an IdP restart is routine, an application restart is not.
func TestResolveSucceedsWhenTheIdPComesBack(t *testing.T) {
	h := auth.NewProviderHolder()
	var calls atomic.Int32

	// Down first.
	down := errors.New("down")
	if _, err := h.Resolve(context.Background(), issuer{}, discoverFunc(&calls, down, nil), time.Nanosecond); err == nil {
		t.Fatal("want an error while down")
	}

	// Back: with a zero interval the retry is allowed immediately.
	want := &auth.Provider{}
	got, err := h.Resolve(context.Background(), issuer{}, discoverFunc(&calls, nil, want), 0)
	if err != nil {
		t.Fatalf("discovery should now succeed: %v", err)
	}
	if got != want {
		t.Fatal("Resolve must return the discovered provider")
	}

	// And it is cached: later calls do not rediscover, and a cached provider survives a
	// later failure being reported for an unrelated request.
	before := calls.Load()
	if _, err := h.Resolve(context.Background(), issuer{}, discoverFunc(&calls, down, nil), 0); err != nil {
		t.Fatalf("a cached provider must be returned without discovery: %v", err)
	}
	if calls.Load() != before {
		t.Fatal("a cached provider must not trigger another discovery")
	}
}

// TestSetThenGet: an explicit startup success is visible immediately.
func TestSetThenGet(t *testing.T) {
	h := auth.NewProviderHolder()
	p := &auth.Provider{}
	h.Set(p)

	got, ok := h.Get()
	if !ok || got != p {
		t.Fatal("Set must make the provider available")
	}
	if h.LastError() != nil {
		t.Fatal("a successful Set must clear the previous error")
	}
	if h.ShouldRetry(0) {
		t.Fatal("a holder with a provider must not report that a retry is due")
	}
}

// TestShouldRetryAfterInterval.
func TestShouldRetryAfterInterval(t *testing.T) {
	h := auth.NewProviderHolder()
	_ = h.ShouldRetry(0) // no attempt yet

	// Record a failure.
	var calls atomic.Int32
	_, _ = h.Resolve(context.Background(), issuer{}, discoverFunc(&calls, errors.New("x"), nil), 0)

	if h.ShouldRetry(time.Hour) {
		t.Fatal("a retry immediately after a failure must be rate-limited")
	}
	if !h.ShouldRetry(0) {
		t.Fatal("a zero interval means retries are always allowed")
	}
}

// TestConcurrentResolveDoesNotStampede: two logins arriving together must not both run
// discovery, which under an outage means two timeouts instead of one.
func TestConcurrentResolveDoesNotStampede(t *testing.T) {
	h := auth.NewProviderHolder()
	var calls atomic.Int32

	want := &auth.Provider{}
	discover := func(context.Context) (*auth.Provider, error) {
		calls.Add(1)
		// A small delay widens the race window.
		time.Sleep(20 * time.Millisecond)
		return want, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.Resolve(context.Background(), issuer{}, discover, time.Hour)
		}()
	}
	wg.Wait()

	// The first caller discovers; the rest either see the write lock held and are rate-limited,
	// or find the provider already set. Either way it is not eight discoveries.
	if n := calls.Load(); n > 2 {
		t.Fatalf("concurrent callers must not stampede discovery, ran %d", n)
	}
}
