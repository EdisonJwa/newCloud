package server_test

import (
	"net/http"
	"testing"
)

func TestSetNotReadyFailsReadinessImmediately(t *testing.T) {
	h := newHarness(t)
	resp, err := http.Get(h.srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthy readyz = %d, want 200", resp.StatusCode)
	}

	h.srvServer.SetNotReady()
	resp, err = http.Get(h.srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("after SetNotReady, readyz = %d, want 503", resp.StatusCode)
	}
}
