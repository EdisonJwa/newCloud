package nccompat

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// Login flow v2 is how the official Nextcloud desktop and mobile clients obtain a
// credential without ever handling the user's password (ADR-0015):
//
//  1. the client POSTs to /index.php/login/v2 with no credentials and receives two
//     independent secrets — a POLL token it keeps, and a LOGIN URL it opens in the
//     user's browser;
//  2. the browser signs in through the normal OIDC flow and, on the approval page,
//     authorises the named client;
//  3. the client polls /login/v2/poll with its own token until the approval lands, then
//     receives a freshly minted App Password and closes the loop.
//
// The two tokens are separate on purpose. If the browser's URL carried the poll token, a
// browser history sync or an extension could redeeming the credential the user was
// approving for a different application. Only the SHA-256 of either token is stored.

// HandleLoginFlowStart serves POST /index.php/login/v2.
func (h *Handler) HandleLoginFlowStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONStatus(w, http.StatusMethodNotAllowed, map[string]string{"message": "method not allowed"})
		return
	}
	if h.deps.Meta == nil || h.deps.PublicURL == "" {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"message": "login flow unavailable"})
		return
	}

	pollToken, err := randomToken()
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"message": "cannot start flow"})
		return
	}
	loginToken, err := randomToken()
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"message": "cannot start flow"})
		return
	}

	now := h.now()
	flow := store.LoginFlow{
		ID:         uuid.New(),
		ClientName: shortClientName(r.UserAgent()),
		CreatedAt:  now,
		ExpiresAt:  now.Add(loginFlowTTL),
	}
	// The housekeeping runs in the same transaction as the insert: it costs nothing to
	// append, and it means the table cannot grow unbounded from abandoned flows just
	// because the worker happens to be behind.
	err = h.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		if _, derr := tx.DeleteExpiredLoginFlows(ctx, now); derr != nil {
			return derr
		}
		return tx.CreateLoginFlow(ctx, flow, tokenHash(pollToken), tokenHash(loginToken))
	})
	if err != nil {
		h.log.Error("nextcloud login flow: cannot create flow", "error", err)
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"message": "cannot start flow"})
		return
	}

	// The response shape is fixed by the clients: "endpoint" is the URL they poll, and
	// "login" is what they open in the default browser. Both are absolute and are built
	// from the configured public URL, not from Host, so a proxied deployment advertises
	// the address users can actually reach.
	writeJSONStatus(w, http.StatusOK, map[string]any{
		"poll": map[string]string{
			"token":    pollToken,
			"endpoint": h.deps.PublicURL + "/login/v2/poll",
		},
		"login": h.deps.PublicURL + "/login/v2/flow/" + url.PathEscape(loginToken),
	})
}

// pollResult is the credential reply. Field names are the protocol.
type pollResult struct {
	Server      string `json:"server"`
	LoginName   string `json:"loginName"`
	AppPassword string `json:"appPassword"`
}

// HandleLoginFlowPoll serves POST /login/v2/poll (and the /index.php alias).
//
// Until the browser approves, the answer is 404 with no body — that is what the client
// treats as "keep waiting". Once approved, the first poll deletes the flow and returns
// the credential; any later poll is 404.
func (h *Handler) HandleLoginFlowPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// A poll body is one small form field. Cap it before parsing.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// FormValue reads the query string as well as the body. The documented call posts the
	// token as a form field; accepting it in the query costs nothing and some client
	// libraries put it there.
	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	now := h.now()
	var flow store.LoginFlow
	err := h.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		got, cerr := tx.ConsumeLoginFlow(ctx, tokenHash(token), now)
		if cerr != nil {
			return cerr
		}
		flow = got
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		// Not approved yet, already collected, or never existed. The client polls this
		// for up to twenty minutes; the three cases are deliberately indistinguishable.
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		h.log.Error("nextcloud login flow: poll failed", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if flow.UserID == nil {
		// Unreachable with the store's predicate (it only consumes approved rows), but a
		// nil here would otherwise mean "mint a credential for nobody".
		w.WriteHeader(http.StatusNotFound)
		return
	}
	user, err := h.userByID(r.Context(), *flow.UserID)
	if err != nil {
		h.log.Error("nextcloud login flow: approved account is gone", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// The credential is minted HERE, not at approval time, and its audit row commits with
	// it. Creating it earlier would mean holding a usable secret for a client that may
	// never poll, and storing it would defeat the reason App Passwords are hashed.
	label := flow.ClientName
	if label == "" {
		label = "Nextcloud client"
	}
	secret, _, err := h.deps.AppPass.IssueAudited(r.Context(), user.ID, label, "rw", 0,
		store.AuditEntry{
			ActorKind: "user", ActorUserID: &user.ID, ActorEmail: user.Email,
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "auth.apppassword.create",
			Metadata: map[string]any{
				"scope": "rw", "label": label, "via": "nextcloud-login-flow-v2",
			},
		})
	if err != nil {
		// The flow was already consumed, so this client cannot retry the poll. It has to
		// start again from the browser; logged loudly because it is otherwise invisible.
		h.log.Error("nextcloud login flow: issuing the app password failed after the flow was consumed",
			"user", user.ID, "error", err)
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"message": "cannot issue credential"})
		return
	}

	writeJSONStatus(w, http.StatusOK, pollResult{
		Server:      h.deps.PublicURL,
		LoginName:   namespaceOf(user),
		AppPassword: secret,
	})
}

// HandleLoginFlowApprove serves the browser half: GET renders the approval page, POST
// records the approval. Both require an ordinary OIDC session, which is the entire point
// of the flow — the browser is where SSO already works.
func (h *Handler) HandleLoginFlowApprove(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		h.writePage(w, http.StatusNotFound, pageData{Message: "This sign-in link is incomplete."})
		return
	}

	user, ok := h.browserUser(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		// Same-origin check first: SameSite=Lax already withholds the session cookie
		// from a cross-site POST, and this refuses the request explicitly rather than
		// relying on the cookie jar alone.
		if !h.sameOrigin(r) {
			h.writePage(w, http.StatusForbidden, pageData{Message: "This request did not come from this server."})
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		h.writePage(w, http.StatusMethodNotAllowed, pageData{Message: "Method not allowed."})
		return
	}

	var (
		flow store.LoginFlow
		err  error
	)
	if r.Method == http.MethodGet {
		err = h.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
			got, lerr := tx.LoginFlowByLoginHash(ctx, tokenHash(token))
			flow = got
			return lerr
		})
	} else {
		err = h.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
			got, aerr := tx.ApproveLoginFlow(ctx, tokenHash(token), user.ID, h.now())
			if aerr != nil {
				return aerr
			}
			flow = got
			return nil
		})
	}

	switch {
	case errors.Is(err, store.ErrLoginFlowConflict):
		h.writePage(w, http.StatusConflict, pageData{
			Message: "This sign-in link has already been approved by a different account.",
		})
		return
	case errors.Is(err, store.ErrNotFound):
		h.writePage(w, http.StatusNotFound, pageData{
			Message: "This sign-in link is invalid or has expired. Start the sign-in again from the application.",
		})
		return
	case err != nil:
		h.log.Error("nextcloud login flow: approval page failed", "error", err)
		h.writePage(w, http.StatusInternalServerError, pageData{Message: "The server could not complete the request."})
		return
	}

	if flow.UserID != nil && *flow.UserID != user.ID {
		h.writePage(w, http.StatusConflict, pageData{
			Message: "This sign-in link has already been approved by a different account.",
		})
		return
	}

	data := pageData{
		Client:  flow.ClientName,
		Account: displayNameOf(user),
		Login:   namespaceOf(user),
	}
	if r.Method == http.MethodPost {
		data.Done = true
	}
	h.writePage(w, http.StatusOK, data)
}

// browserUser resolves the OIDC session, redirecting a GET to the login page when there
// is none. A POST with no session is answered rather than redirected: a redirect would
// replay the approval form's intent against a page that does not accept it.
func (h *Handler) browserUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	if h.deps.SessionUser == nil {
		h.writePage(w, http.StatusServiceUnavailable, pageData{Message: "Sign-in is not configured."})
		return store.User{}, false
	}
	user, err := h.deps.SessionUser(r)
	if err == nil {
		if h.deps.Stale != nil && h.deps.Stale(user) {
			err = ErrUnauthenticated
		} else {
			return user, true
		}
	}
	if r.Method == http.MethodGet {
		next := r.URL.RequestURI()
		// isSameSitePath is enforced by the login handler itself; encoding here keeps a
		// query string from breaking the redirect.
		http.Redirect(w, r, "/api/auth/login?next="+url.QueryEscape(next), http.StatusFound)
		return store.User{}, false
	}
	h.writePage(w, http.StatusUnauthorized, pageData{
		Message: "Your sign-in session has expired. Open this link again in your browser.",
	})
	return store.User{}, false
}

// sameOrigin reports whether a POST's Origin names this server. An absent Origin (an
// older client) is accepted: SameSite=Lax is still in force for it.
func (h *Handler) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	got, err := url.Parse(origin)
	if err != nil {
		return false
	}
	pub, err := url.Parse(h.deps.PublicURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(got.Host, pub.Host)
}

// userByID loads an account by id through the store's transaction surface.
func (h *Handler) userByID(ctx context.Context, id uuid.UUID) (store.User, error) {
	var user store.User
	err := h.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, uerr := tx.UserByID(ctx, id)
		user = got
		return uerr
	})
	return user, err
}

// pageData is the input to every page this package renders.
type pageData struct {
	Client  string
	Account string
	Login   string
	Message string
	Done    bool
}

// flowPage is deliberately plain: no script, no external resources, and the global CSP
// already forbids both. It is a consent screen, so it says exactly what will happen.
var flowPage = template.Must(template.New("flow").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .Done}}Access granted{{else if .Message}}Sign-in problem{{else}}Authorize {{.Client}}{{end}}</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 16px/1.5 system-ui, sans-serif; margin: 0; display: grid; place-items: center;
         min-height: 100vh; background: #f5f7fa; color: #14202e; }
  main { max-width: 34rem; margin: 2rem; padding: 2rem; background: #fff; border-radius: 12px;
         box-shadow: 0 10px 30px rgba(20,32,46,.12); }
  h1 { font-size: 1.4rem; margin-top: 0; }
  .who { padding: .75rem 1rem; background: #eef3fa; border-radius: 8px; margin: 1rem 0; }
  .who strong { display: block; }
  button { font: inherit; padding: .6rem 1.2rem; border: 0; border-radius: 8px;
           background: #0b5cad; color: #fff; cursor: pointer; }
  button:hover { background: #094a8c; }
  a { color: #0b5cad; }
  .muted { color: #5b6b7c; font-size: .92rem; }
</style></head>
<body><main>
{{if .Message}}
  <h1>Sign-in problem</h1>
  <p>{{.Message}}</p>
  <p class="muted">You can close this window.</p>
{{else if .Done}}
  <h1>Access granted</h1>
  <p>{{.Account}} has authorized <strong>{{.Client}}</strong>.</p>
  <p>You can close this window and return to the application; it will finish signing in by itself.</p>
  <p class="muted">An app password named <strong>{{.Client}}</strong> was created for it. You can revoke it under Account &rsaquo; App passwords.</p>
{{else}}
  <h1>Authorize {{.Client}}</h1>
  <p><strong>{{.Client}}</strong> is asking for access to your files.</p>
  <div class="who">
    <strong>{{.Account}}</strong>
    <span class="muted">{{.Login}}</span>
  </div>
  <p>Approving creates an app password for this application alone. The application never
     receives your password, and the credential can be revoked at any time without
     affecting your other devices.</p>
  <form method="post">
    <button type="submit">Authorize {{.Client}}</button>
  </form>
  <p class="muted">If you did not start this from an application you trust, close this window.</p>
{{end}}
</main></body></html>
`))

// writePage renders the consent/result page with the right status and headers.
func (h *Handler) writePage(w http.ResponseWriter, status int, data pageData) {
	if data.Client == "" {
		data.Client = "the application"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = flowPage.Execute(w, data)
}

// writeJSONStatus emits a small JSON body with the given status. The login-flow start and
// poll endpoints are JSON-only by protocol.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
