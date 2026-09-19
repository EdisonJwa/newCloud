package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/share"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// shareJSON is the owner-facing view of a share.
//
// It never contains the token: the owner already has the link, and re-exposing
// tokens through the API would put working credentials in every list response.
type shareJSON struct {
	ID string `json:"id"`
	// NodeID lets the file inspector answer "is this file shared, and by which links".
	NodeID        string     `json:"node_id"`
	Kind          string     `json:"kind"`
	Label         string     `json:"label"`
	URL           string     `json:"url,omitempty"`
	NodePath      string     `json:"node_path"`
	HasPassword   bool       `json:"has_password"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	MaxDownloads  *int       `json:"max_downloads,omitempty"`
	DownloadCount int        `json:"download_count"`
	MaxUploads    *int       `json:"max_uploads,omitempty"`
	UploadCount   int        `json:"upload_count"`
	Permissions   []string   `json:"permissions"`
	HideContents  bool       `json:"hide_contents"`
	Recipients    []string   `json:"recipients,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
}

// handleShareList lists the caller's shares.
func (s *Server) handleShareList(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var shares []store.Share
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		list, lerr := tx.SharesByOwner(ctx, actor.ID())
		shares = list
		return lerr
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	out := make([]shareJSON, 0, len(shares))
	for _, sh := range shares {
		out = append(out, s.toShareJSON(r.Context(), sh))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleShareCreate issues a share.
func (s *Server) handleShareCreate(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		Path         string   `json:"path"`
		Kind         string   `json:"kind"`
		Label        string   `json:"label"`
		Password     string   `json:"password"`
		ExpiresIn    int      `json:"expires_in_seconds"`
		MaxDownloads *int     `json:"max_downloads"`
		MaxUploads   *int     `json:"max_uploads"`
		Permissions  []string `json:"permissions"`
		Recipients   []string `json:"recipients"`
		// The per-share upload limits. They were enforced on the read path but
		// could not be set here, so a drop box could never actually be limited —
		// decodeJSON rejects unknown fields, so sending them was a 400.
		MaxBytesPerFile  *int64 `json:"max_bytes_per_file"`
		MaxTotalBytes    *int64 `json:"max_total_bytes"`
		RetentionSeconds *int64 `json:"retention_seconds"`
		// The rate limit (SHARE-05 速率). Both are needed: a count without a window would
		// mean "per hour" by an unstated convention, and a window without a count means
		// nothing.
		MaxUploadRate           *int64   `json:"max_upload_rate"`
		UploadRateWindowSeconds *int64   `json:"upload_rate_window_seconds"`
		AllowedMIME             []string `json:"allowed_mime"`
		// HideContents is accepted but not trusted: the service forces it on for request
		// shares. It is in the struct anyway because the client sends it, and
		// decodeJSON rejects unknown fields — omitting it here made every "create a drop
		// box" request fail with 400 from the bundled frontend.
		HideContents bool `json:"hide_contents"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), body.Path)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	node := nodes[len(nodes)-1]

	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	// Sharing is a separate permission from writing: a group editor who cannot
	// share must not gain that power by creating a link.
	if !cap.Can(authz.PermShare) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	kind := store.ShareKind(body.Kind)
	if kind == "" {
		kind = store.ShareLink
	}
	switch kind {
	case store.ShareLink, store.ShareEmail, store.ShareRequest:
	default:
		writeError(w, http.StatusBadRequest, "invalid_kind", "kind must be link, email or request")
		return
	}

	for _, addr := range body.Recipients {
		if err := share.ValidateRecipient(addr); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_recipient", err.Error())
			return
		}
	}

	var ttl time.Duration
	if body.ExpiresIn > 0 {
		ttl = time.Duration(body.ExpiresIn) * time.Second
	}

	created, err := s.deps.Shares.Create(r.Context(), share.NewShare{
		NodeID: node.ID, OwnerID: actor.ID(), OwnerEmail: actor.Email(), NodePath: strings.TrimPrefix(body.Path, "/"), Kind: kind,
		Label: body.Label, Password: body.Password, TTL: ttl,
		MaxDownloads: body.MaxDownloads, MaxUploads: body.MaxUploads,
		MaxBytesPerFile: body.MaxBytesPerFile, MaxTotalBytes: body.MaxTotalBytes, RetentionSeconds: body.RetentionSeconds, MaxUploadRate: body.MaxUploadRate, UploadRateWindowSeconds: body.UploadRateWindowSeconds, AllowedMIME: body.AllowedMIME,
		Permissions: body.Permissions, Recipients: body.Recipients,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	// The plaintext token is returned exactly once, here.
	out := s.toShareJSON(r.Context(), created.Share)
	out.URL = s.shareURL(created.Token)
	writeJSON(w, http.StatusCreated, map[string]any{
		"share": out,
		"token": created.Token,
		"note":  "this link is shown once; store it now",
	})
}

// handleShareUpdate edits a share.
func (s *Server) handleShareUpdate(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	shareID := parseUUID(chiURLParam(r, "id"))

	var body struct {
		Label        *string `json:"label"`
		Password     *string `json:"password"`
		ExpiresIn    *int    `json:"expires_in_seconds"`
		MaxDownloads *int    `json:"max_downloads"`
		MaxUploads   *int    `json:"max_uploads"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	var sh store.Share
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		cur, gerr := tx.ShareByID(ctx, shareID)
		if gerr != nil {
			return gerr
		}
		// Only the owner may edit: a recipient with the link must not be able to
		// extend their own access.
		if cur.OwnerID != actor.ID() {
			return authz.ErrDenied
		}
		if body.Label != nil {
			cur.Label = *body.Label
		}
		if body.Password != nil {
			h, herr := s.deps.Shares.HashPassword(*body.Password)
			if herr != nil {
				return herr
			}
			cur.PasswordHash = h
		}
		if body.ExpiresIn != nil {
			if *body.ExpiresIn <= 0 {
				cur.ExpiresAt = nil
			} else {
				t := time.Now().Add(time.Duration(*body.ExpiresIn) * time.Second)
				cur.ExpiresAt = &t
			}
		}
		if body.MaxDownloads != nil {
			cur.MaxDownloads = body.MaxDownloads
		}
		if body.MaxUploads != nil {
			cur.MaxUploads = body.MaxUploads
		}

		updated, uerr := tx.UpdateShare(ctx, cur)
		if uerr != nil {
			return uerr
		}
		sh = updated
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "share.update", TargetKind: "share", TargetID: shareID.String(),
			Result: "ok",
		})
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toShareJSON(r.Context(), sh))
}

// handleShareRevoke withdraws a share.
func (s *Server) handleShareRevoke(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	shareID := parseUUID(chiURLParam(r, "id"))

	// Ownership is checked here rather than inside the store so the store stays
	// free of authorization decisions.
	allowed := false
	_ = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		cur, err := tx.ShareByID(ctx, shareID)
		if err != nil {
			return err
		}
		allowed = cur.OwnerID == actor.ID()
		return nil
	})
	if !allowed {
		writeError(w, http.StatusNotFound, "not_found", "no such share")
		return
	}

	if err := s.deps.Shares.Revoke(r.Context(), shareID, actor.ID(), actor.Email()); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// toShareJSON builds the owner-facing view.
func (s *Server) toShareJSON(ctx context.Context, sh store.Share) shareJSON {
	out := shareJSON{
		ID: sh.ID.String(), NodeID: sh.NodeID.String(), Kind: string(sh.Kind), Label: sh.Label,
		HasPassword: sh.NeedsPassword(), ExpiresAt: sh.ExpiresAt,
		MaxDownloads: sh.MaxDownloads, DownloadCount: sh.DownloadCount,
		MaxUploads: sh.MaxUploads, UploadCount: sh.UploadCount,
		Permissions: sh.Permissions, HideContents: sh.HideContents,
		CreatedAt: sh.CreatedAt, RevokedAt: sh.RevokedAt,
	}

	// The path makes the list readable ("/docs/report.pdf" rather than a UUID). It
	// used to resolve the owner's *root* instead of the shared node, so the field
	// was always empty — a silent placeholder is worse than an absent one.
	//
	// Best-effort: a resolution failure must not break the whole listing, and a
	// file that was renamed since the share was made simply shows its new path.
	out.NodePath = s.pathOfShare(ctx, sh)

	_ = s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		recs, err := tx.RecipientsFor(ctx, sh.ID)
		if err != nil {
			return err
		}
		for _, rec := range recs {
			out.Recipients = append(out.Recipients, rec.Email)
		}
		return nil
	})
	return out
}

// pathOfShare reconstructs the shared node's path, or "" when it is gone.
func (s *Server) pathOfShare(ctx context.Context, sh store.Share) string {
	var name string
	var parent *uuid.UUID
	err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.Node(ctx, sh.NodeID)
		if nerr != nil {
			return nerr
		}
		name, parent = n.Name, n.ParentID
		return nil
	})
	if err != nil || parent == nil {
		// A root has no path of its own; the caller sees the namespace root.
		return ""
	}

	segs := []string{name}
	id := *parent
	for depth := 0; depth < 256; depth++ {
		var n store.Node
		ferr := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			got, gerr := tx.Node(ctx, id)
			n = got
			return gerr
		})
		if ferr != nil || n.ParentID == nil {
			break
		}
		segs = append(segs, n.Name)
		id = *n.ParentID
	}
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}
	return "/" + strings.Join(segs, "/")
}

func (s *Server) shareURL(token string) string {
	return strings.TrimSuffix(s.deps.Config.Server.PublicURL, "/") + "/s/" + token
}

// handleAppPasswordList lists the caller's credentials.
func (s *Server) handleAppPasswordList(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	list, err := s.deps.AppPass.List(r.Context(), actor.ID())
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		out = append(out, map[string]any{
			"id": p.ID, "label": p.Label, "prefix": p.Prefix, "scope": p.Scope,
			"created_at": p.CreatedAt, "expires_at": p.ExpiresAt,
			"last_used": p.LastUsed, "revoked": p.RevokedAt != nil,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleAppPasswordCreate issues a credential for a desktop client.
//
// This is the only way a WebDAV client can authenticate against an SSO-only
// account, so it is a core feature rather than a convenience.
func (s *Server) handleAppPasswordCreate(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		Label     string `json:"label"`
		Scope     string `json:"scope"`
		ExpiresIn int    `json:"expires_in_seconds"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Scope == "" {
		body.Scope = "rw"
	}

	var ttl time.Duration
	if body.ExpiresIn > 0 {
		ttl = time.Duration(body.ExpiresIn) * time.Second
	}

	// The audit event is written in the same transaction as the credential. It used to be a
	// best-effort write afterwards, so a failure left a live credential with no record of who
	// created it — while the client was told the creation succeeded.
	secret, rec, err := s.deps.AppPass.IssueAudited(r.Context(), actor.ID(), body.Label,
		body.Scope, ttl, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action:   "auth.apppassword.create",
			Metadata: map[string]any{"scope": body.Scope, "label": body.Label},
		})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	// The plaintext appears exactly once. A UI that offers "show it again" would
	// require storing it, which defeats the point.
	writeJSON(w, http.StatusCreated, map[string]any{
		"secret": secret,
		"id":     rec.ID,
		"prefix": rec.Prefix,
		"note":   "store this now; it cannot be shown again",
	})
}

// handleAppPasswordRevoke kills a credential.
func (s *Server) handleAppPasswordRevoke(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	id := parseUUID(chiURLParam(r, "id"))

	// Only the owner may revoke, verified by checking the list rather than trusting
	// the id in the URL.
	owned := false
	list, err := s.deps.AppPass.List(r.Context(), actor.ID())
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	for _, p := range list {
		if p.ID == id {
			owned = true
			break
		}
	}
	if !owned {
		writeError(w, http.StatusNotFound, "not_found", "no such credential")
		return
	}

	// Revocation and its audit event commit together, for the same reason creation does.
	if err := s.deps.AppPass.RevokeAudited(r.Context(), id, store.AuditEntry{
		ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
		ActorIP: clientIP(r), UserAgent: r.UserAgent(),
		Action: "auth.apppassword.revoke",
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var _ = io.Copy
var _ = strconv.Itoa
var _ = path.Join
var _ = uuid.Nil
var _ = fmt.Sprintf
