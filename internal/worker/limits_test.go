package worker_test

// The limitations page claims a specific size for the job catalogue.
//
// "The queue has leases, retries, backoff, and three registered job kinds."
//
// That sentence is the reason a reader trusts the rest of the page: it admits the catalogue is
// small. If it drifted — a kind removed and the number left, or a kind added and the page left
// saying three — the page would be wrong in the direction that makes the product look bigger than
// it is, which is the direction that matters least operationally and most for trust.
//
// Counted through the worker's own registry rather than by reading the source, so a kind registered
// at boot from somewhere else is included.

import (
	"testing"

	"github.com/EdisonJwa/newCloud/internal/worker"
)

func TestLimitationsJobKindCountMatchesTheWorker(t *testing.T) {
	w := worker.NewForTest()
	kinds := w.JobKinds()

	if len(kinds) != 3 {
		t.Fatalf("docs/guides/limitations.md says three registered job kinds; the worker has %d: %v",
			len(kinds), kinds)
	}
	// Named, so a replacement that keeps the count but changes the catalogue still shows up in the
	// failure message rather than passing on a coincidence.
	want := map[string]bool{
		"blob.collect":           true,
		"thumbnail.warm":         true,
		"user.purge_credentials": true,
	}
	for _, k := range kinds {
		if !want[k] {
			t.Fatalf("job kind %q is registered but not described on the limitations page", k)
		}
		delete(want, k)
	}
	for k := range want {
		t.Fatalf("the limitations page describes job kind %q, which is no longer registered", k)
	}
}
