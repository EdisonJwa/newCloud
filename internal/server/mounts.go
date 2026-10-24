package server

// The mount management routes (ADR-0014): list, create, unmount.
//
// These are the only routes that change what "mounted storage" means on a deployment, so
// they are also where the operator-facing rules surface: who may create a mount, what a
// mount may point at, and what creation refuses to do silently.

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/mount"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// mountJSON is one mount as the API renders it. The credential never appears: it exists
// only as the fact that one was stored.
type mountJSON struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Scheme       string  `json:"scheme"`
	Endpoint     string  `json:"endpoint"`
	RootPath     string  `json:"root_path"`
	Username     string  `json:"username"`
	ReadOnly     bool    `json:"read_only"`
	OwnerID      string  `json:"owner_id"`
	HealthStatus string  `json:"health_status"`
	HealthDetail string  `json:"health_detail,omitempty"`
	HealthAt     *string `json:"health_at,omitempty"`
}

// actorStoreUser projects the request's actor into the store's user shape the mount
// service authorizes against. The service needs only identity and role; inventing more
// here would imply the mount layer trusts fields it does not read.
func actorStoreUser(a authz.Actor) store.User {
	return store.User{ID: a.ID(), Email: a.Email(), Role: a.Role().String()}
}

func (s *Server) handleMountList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Mounts == nil {
		writeError(w, http.StatusServiceUnavailable, "mounts_disabled", "mounts are not configured on this deployment")
		return
	}
	actor, _ := actorFrom(r.Context())
	mounts, err := s.deps.Mounts.List(r.Context(), actorStoreUser(actor))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	out := make([]mountJSON, 0, len(mounts))
	for _, m := range mounts {
		item := mountJSON{
			ID: m.ID.String(), Name: m.Name, Scheme: m.Scheme, Endpoint: m.Endpoint,
			RootPath: m.RootPath, Username: m.Username, ReadOnly: m.ReadOnly,
			OwnerID: m.OwnerID.String(), HealthStatus: m.HealthStatus, HealthDetail: m.HealthDetail,
		}
		if m.HealthAt != nil {
			at := m.HealthAt.UTC().Format("2006-01-02T15:04:05Z")
			item.HealthAt = &at
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleMountCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Mounts == nil {
		writeError(w, http.StatusServiceUnavailable, "mounts_disabled", "mounts are not configured on this deployment")
		return
	}
	var body struct {
		Name     string `json:"name"`
		Scheme   string `json:"scheme"`
		Endpoint string `json:"endpoint"`
		RootPath string `json:"root_path"`
		Username string `json:"username"`
		Password string `json:"password"`
		ReadOnly bool   `json:"read_only"`
		// Verify defaults on: a mount created against a dead remote appears in listings
		// as a folder that 502s, which is a support ticket wearing a folder icon.
		Verify *bool `json:"verify"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	verify := true
	if body.Verify != nil {
		verify = *body.Verify
	}
	actor, _ := actorFrom(r.Context())
	m, err := s.deps.Mounts.Create(r.Context(), actorStoreUser(actor), mount.CreateInput{
		Name: body.Name, Scheme: body.Scheme, Endpoint: body.Endpoint, RootPath: body.RootPath,
		Username: body.Username, Password: body.Password, ReadOnly: body.ReadOnly, Verify: verify,
	})
	if err != nil {
		s.writeMountError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, mountJSON{
		ID: m.ID.String(), Name: m.Name, Scheme: m.Scheme, Endpoint: m.Endpoint,
		RootPath: m.RootPath, Username: m.Username, ReadOnly: m.ReadOnly,
		OwnerID: m.OwnerID.String(), HealthStatus: m.HealthStatus,
	})
}

func (s *Server) handleMountDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.Mounts == nil {
		writeError(w, http.StatusServiceUnavailable, "mounts_disabled", "mounts are not configured on this deployment")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "the mount id is not a uuid")
		return
	}
	actor, _ := actorFrom(r.Context())
	if err := s.deps.Mounts.Unmount(r.Context(), actorStoreUser(actor), id); err != nil {
		s.writeMountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeMountError maps the service's typed refusals onto statuses once.
func (s *Server) writeMountError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mount.ErrDisabled):
		writeError(w, http.StatusServiceUnavailable, "mounts_disabled", err.Error())
	case errors.Is(err, mount.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "mounts may only be created by an administrator on this deployment")
	case errors.Is(err, mount.ErrBadScheme), errors.Is(err, mount.ErrBadEndpoint), errors.Is(err, mount.ErrPrivateAddr):
		writeError(w, http.StatusBadRequest, "invalid_mount", err.Error())
	case errors.Is(err, mount.ErrUnreachable):
		writeError(w, http.StatusBadGateway, "unreachable", err.Error())
	case errors.Is(err, mount.ErrNotOwner), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such mount")
	case errors.Is(err, mount.ErrNoCipher):
		writeError(w, http.StatusInternalServerError, "no_secret", err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "a folder with that name already exists")
	default:
		writeError(w, http.StatusInternalServerError, "mount_failed", err.Error())
	}
}
