package server

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// chiURLParam returns a path parameter from the chi route.
func chiURLParam(r *http.Request, key string) string {
	return chi.URLParam(r, key)
}

// notImplemented answers a feature that exists in the routing table but not yet in
// behaviour.
//
// Returning 501 rather than an empty 200 matters: a client that gets a plausible
// empty response cannot tell the difference between "nothing here" and "this is
// not built", and will act on the wrong conclusion.
func (s *Server) notImplemented(w http.ResponseWriter, r *http.Request, feature string) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error":     "not_implemented",
		"feature":   feature,
		"milestone": "M1",
	})
}

var _ = strings.TrimSpace
