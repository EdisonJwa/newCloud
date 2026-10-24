package nccompat

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// The OCS API is Nextcloud's REST envelope: every response is
// {"ocs": {"meta": {...}, "data": {...}}} (or the XML equivalent), and the version is
// part of the path (/ocs/v1.php/..., /ocs/v2.php/...). The only differences that matter
// to a client are the success status code inside meta (100 for v1, 200 for v2) and the
// default format (XML unless ?format=json).
//
// This server implements three endpoints, which is what the clients actually call:
//
//	GET /ocs/{v1,v2}.php/cloud/capabilities   what the server can do
//	GET /ocs/{v1,v2}.php/cloud/user           who the caller is, and their quota
//
// The envelope and the header requirement are reproduced exactly, because a client that
// gets them wrong concludes the server is broken rather than minimal.

// ocsMeta is the status block every OCS response carries.
type ocsMeta struct {
	Status     string `json:"status" xml:"status"`
	StatusCode int    `json:"statuscode" xml:"statuscode"`
	Message    string `json:"message" xml:"message"`
}

// ocsEnvelope is the response shape. Data is a pointer so an empty result serialises as
// null/empty rather than being omitted.
type ocsEnvelope struct {
	XMLName xml.Name `json:"-" xml:"ocs"`
	Meta    ocsMeta  `json:"meta" xml:"meta"`
	Data    any      `json:"data" xml:"data"`
}

// Status is the body of GET /status.php, the first request every Nextcloud client makes.
//
// The field names are the protocol: "needsDbUpgrade" in particular is camel-cased, and a
// client that cannot parse this document refuses the server before it ever tries to log
// in. The values are honest for this deployment: it is installed, it is not in
// maintenance, and its schema is migrated at startup rather than by an upgrade button.
type status struct {
	Installed       bool   `json:"installed"`
	Maintenance     bool   `json:"maintenance"`
	NeedsDBUpgrade  bool   `json:"needsDbUpgrade"`
	Version         string `json:"version"`
	VersionString   string `json:"versionstring"`
	Edition         string `json:"edition"`
	ProductName     string `json:"productname"`
	ExtendedSupport bool   `json:"extendedSupport"`
}

// HandleStatus serves GET /status.php.
func (h *Handler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_ = json.NewEncoder(w).Encode(status{
		Installed:     true,
		Version:       versionFull,
		VersionString: ProtocolVersion,
		ProductName:   productName,
	})
}

// versionInfo is the version block of the capabilities response.
type versionInfo struct {
	Major           int    `json:"major" xml:"major"`
	Minor           int    `json:"minor" xml:"minor"`
	Micro           int    `json:"micro" xml:"micro"`
	String          string `json:"string" xml:"string"`
	Edition         string `json:"edition" xml:"edition"`
	ExtendedSupport bool   `json:"extendedSupport" xml:"extendedSupport"`
}

// capabilitiesData is data for cloud/capabilities.
type capabilitiesData struct {
	Version      versionInfo  `json:"version" xml:"version"`
	Capabilities capabilities `json:"capabilities" xml:"capabilities"`
}

type capabilities struct {
	Core    coreCapabilities    `json:"core" xml:"core"`
	Files   filesCapabilities   `json:"files" xml:"files"`
	Theming themingCapabilities `json:"theming" xml:"theming"`
}

type coreCapabilities struct {
	PollInterval int    `json:"pollinterval" xml:"pollinterval"`
	WebDAVRoot   string `json:"webdav-root" xml:"webdav-root"`
}

// filesCapabilities deliberately reports no chunking support.
//
// Nextcloud clients decide whether to split a large upload into per-chunk PUTs from these
// fields. They have two implementations: "bigfilechunking" (chunk number in the URL) and
// the newer "chunked_upload"/dav.chunking 1.0 (a staging collection assembled with MOVE).
// This server implements neither, and advertising one would make every large upload fail
// halfway through instead of being sent as a single PUT. A false here is what makes the
// client choose the one-shot path it already knows works.
type filesCapabilities struct {
	BigFileChunking  bool     `json:"bigfilechunking" xml:"bigfilechunking"`
	BlacklistedFiles []string `json:"blacklisted_files" xml:"blacklisted_files>element"`
}

type themingCapabilities struct {
	Name        string `json:"name" xml:"name"`
	URL         string `json:"url" xml:"url"`
	Slogan      string `json:"slogan" xml:"slogan"`
	Color       string `json:"color" xml:"color"`
	ColorText   string `json:"color-text" xml:"color-text"`
	ColorElem   string `json:"color-element" xml:"color-element"`
	ColorElemBr string `json:"color-element-bright" xml:"color-element-bright"`
	ColorElemDk string `json:"color-element-dark" xml:"color-element-dark"`
}

// quotaInfo is Nextcloud's quota block. -3 is the protocol's "unlimited": clients already
// handle it because a fresh Nextcloud account has no quota either.
type quotaInfo struct {
	Free     int64   `json:"free" xml:"free"`
	Used     int64   `json:"used" xml:"used"`
	Total    int64   `json:"total" xml:"total"`
	Relative float64 `json:"relative" xml:"relative"`
	Quota    int64   `json:"quota" xml:"quota"`
}

const unlimitedQuota = -3

// userData is data for cloud/user.
type userData struct {
	ID          string    `json:"id" xml:"id"`
	DisplayName string    `json:"displayname" xml:"displayname"`
	DisplayHyp  string    `json:"display-name" xml:"display-name"`
	Email       string    `json:"email" xml:"email"`
	Quota       quotaInfo `json:"quota" xml:"quota"`
}

// HandleCapabilities serves GET /ocs/{v1,v2}.php/cloud/capabilities.
func (h *Handler) HandleCapabilities(w http.ResponseWriter, r *http.Request) {
	if !h.ocsAuthorized(w, r) {
		return
	}
	major, minor, micro := parseProtocolVersion()
	h.writeOCS(w, r, http.StatusOK, capabilitiesData{
		Version: versionInfo{
			Major: major, Minor: minor, Micro: micro, String: ProtocolVersion,
		},
		Capabilities: capabilities{
			Core: coreCapabilities{PollInterval: 60, WebDAVRoot: "remote.php/dav"},
			Files: filesCapabilities{
				BigFileChunking: false,
				// Nextcloud refuses to create these names. The check is also enforced when
				// a name is created; advertising it lets the client skip the upload and say
				// why, instead of receiving a 400 for each attempt.
				BlacklistedFiles: []string{".htaccess"},
			},
			Theming: themingCapabilities{
				Name: productName, URL: h.deps.PublicURL, Slogan: "Files, sharing, and a proof of who did what",
				Color: "#0b5cad", ColorText: "#ffffff", ColorElem: "#0b5cad",
				ColorElemBr: "#4a8ad0", ColorElemDk: "#073f78",
			},
		},
	})
}

// HandleUser serves GET /ocs/{v1,v2}.php/cloud/user.
//
// The "id" here is the account's namespace segment, not its internal UUID: the client
// concatenates it into /remote.php/dav/files/<id>/, so any other value produces a DAV
// URL this server would refuse.
func (h *Handler) HandleUser(w http.ResponseWriter, r *http.Request) {
	user, ok := h.ocsAuthorizedUser(w, r)
	if !ok {
		return
	}
	used, err := h.deps.Meta.Usage(r.Context(), user.ID)
	if err != nil {
		h.writeOCSFailure(w, r, http.StatusInternalServerError, "could not read quota")
		return
	}
	h.writeOCS(w, r, http.StatusOK, userData{
		ID:          namespaceOf(user),
		DisplayName: displayNameOf(user),
		DisplayHyp:  displayNameOf(user),
		Email:       user.Email,
		Quota:       quotaFor(user.QuotaBytes, used),
	})
}

// quotaFor renders the quota block. A zero or negative quota is unlimited, which the
// protocol spells -3; everything else reports the real numbers so the client can warn
// before an upload rather than after it.
func quotaFor(quota, used int64) quotaInfo {
	if quota <= 0 {
		return quotaInfo{Free: unlimitedQuota, Used: used, Total: unlimitedQuota,
			Relative: 0, Quota: unlimitedQuota}
	}
	free := quota - used
	if free < 0 {
		free = 0
	}
	rel := 0.0
	if quota > 0 {
		rel = float64(used) / float64(quota) * 100
		rel = float64(int64(rel*100+0.5)) / 100
	}
	return quotaInfo{Free: free, Used: used, Total: quota, Relative: rel, Quota: quota}
}

// namespaceOf is the account's DAV path segment. It must agree with the DAV handler's
// notion of the caller's username, or the client follows a URL the server refuses.
func namespaceOf(u store.User) string {
	if u.Username != "" {
		return u.Username
	}
	return u.ID.String()
}

func displayNameOf(u store.User) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	if u.Username != "" {
		return u.Username
	}
	return u.Email
}

// parseProtocolVersion splits ProtocolVersion into the integers clients compare.
func parseProtocolVersion() (major, minor, micro int) {
	parts := strings.Split(ProtocolVersion, ".")
	get := func(i int) int {
		if i >= len(parts) {
			return 0
		}
		n, _ := strconv.Atoi(parts[i])
		return n
	}
	return get(0), get(1), get(2)
}

// ---- OCS plumbing ----

// ocsAuthorized enforces the OCS preconditions and Basic authentication.
func (h *Handler) ocsAuthorized(w http.ResponseWriter, r *http.Request) bool {
	_, ok := h.ocsAuthorizedUser(w, r)
	return ok
}

// ocsAuthorizedUser resolves the caller of an OCS request.
func (h *Handler) ocsAuthorizedUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	user, _, ok := h.ocsCredential(w, r)
	return user, ok
}

// ocsCredential resolves the caller AND the App Password it authenticated with.
//
// The order is deliberate: the header check comes first, because a cross-site form
// submission can reach an OCS URL and must be refused before any credential is examined —
// that is the CSRF protection the header exists for. 412 is Nextcloud's answer.
func (h *Handler) ocsCredential(w http.ResponseWriter, r *http.Request) (store.User, store.AppPassword, bool) {
	// A deployment without an App Password service (or metadata, in the API-only test
	// harnesses) can still answer status.php, but these endpoints genuinely cannot work.
	// Saying so is better than a nil dereference or a 404 that reads as "no such API".
	if h.deps.AppPass == nil || h.deps.Meta == nil {
		h.writeOCSFailure(w, r, http.StatusServiceUnavailable, "app passwords are not configured")
		return store.User{}, store.AppPassword{}, false
	}
	if !hasOCSRequestHeader(r) {
		h.writeOCSFailure(w, r, http.StatusPreconditionFailed,
			"CSRF check failed: the OCS-APIRequest header is required")
		return store.User{}, store.AppPassword{}, false
	}

	_, pass, ok := r.BasicAuth()
	if !ok || pass == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="newCloud", charset="UTF-8"`)
		h.writeOCSFailure(w, r, http.StatusUnauthorized, "authentication required")
		return store.User{}, store.AppPassword{}, false
	}

	user, cred, err := h.deps.AppPass.Verify(r.Context(), pass, clientIP(r))
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="newCloud", charset="UTF-8"`)
		h.writeOCSFailure(w, r, http.StatusUnauthorized, "invalid app password")
		return store.User{}, store.AppPassword{}, false
	}
	if !user.IsActive() {
		w.Header().Set("WWW-Authenticate", `Basic realm="newCloud", charset="UTF-8"`)
		h.writeOCSFailure(w, r, http.StatusUnauthorized, "account is not active")
		return store.User{}, store.AppPassword{}, false
	}
	if h.deps.Stale != nil && h.deps.Stale(user) {
		w.Header().Set("WWW-Authenticate", `Basic realm="newCloud", charset="UTF-8"`)
		h.writeOCSFailure(w, r, http.StatusUnauthorized, "sign-in must be renewed")
		return store.User{}, store.AppPassword{}, false
	}
	return user, cred, true
}

// HandleAppPasswordDelete serves DELETE /ocs/{v1,v2}.php/core/apppassword.
//
// The desktop client calls this when the user removes the account, so the credential it was
// using stops working instead of remaining valid until the account is disabled. Nextcloud
// documents that a client should proceed even when this fails, so the endpoint is a
// courtesy — but an account removal that leaves a working credential behind is a security
// bug from the user's point of view, which is why it is implemented rather than ignored.
func (h *Handler) HandleAppPasswordDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		w.Header().Set("Allow", "DELETE, POST")
		h.writeOCSFailure(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user, cred, ok := h.ocsCredential(w, r)
	if !ok {
		return
	}
	if err := h.deps.AppPass.RevokeAudited(r.Context(), cred.ID, store.AuditEntry{
		ActorKind: "user", ActorUserID: &user.ID, ActorEmail: user.Email,
		ActorIP: clientIP(r), UserAgent: r.UserAgent(),
		Action:   "auth.apppassword.revoke",
		Metadata: map[string]any{"via": "nextcloud-client-account-removal"},
	}); err != nil {
		h.writeOCSFailure(w, r, http.StatusInternalServerError, "could not revoke the credential")
		return
	}
	h.writeOCS(w, r, http.StatusOK, struct{}{})
}

// ocsVersionCode is the meta status code for a successful call: OCS v1 spells success
// 100 and v2 spells it 200. Clients read it, so it cannot be hard-coded to one.
func ocsVersionCode(r *http.Request) int {
	if strings.Contains(r.URL.Path, "/v2.php") {
		return 200
	}
	return 100
}

// writeOCS emits a successful envelope in the format the client asked for.
func (h *Handler) writeOCS(w http.ResponseWriter, r *http.Request, httpStatus int, data any) {
	env := ocsEnvelope{
		Meta: ocsMeta{Status: "ok", StatusCode: ocsVersionCode(r), Message: "OK"},
		Data: data,
	}
	h.writeEnvelope(w, r, httpStatus, env)
}

// writeOCSFailure emits a failure envelope. The meta status code is the HTTP status,
// which is what clients look at when meta.status is "failure".
func (h *Handler) writeOCSFailure(w http.ResponseWriter, r *http.Request, httpStatus int, msg string) {
	h.writeEnvelope(w, r, httpStatus, ocsEnvelope{
		Meta: ocsMeta{Status: "failure", StatusCode: httpStatus, Message: msg},
		Data: struct{}{},
	})
}

func (h *Handler) writeEnvelope(w http.ResponseWriter, r *http.Request, httpStatus int, env ocsEnvelope) {
	w.Header().Set("Cache-Control", "no-store")
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(httpStatus)
		// The JSON form nests the envelope under "ocs"; the XML form's root element IS
		// <ocs>. Encoding the struct directly would produce {"meta":…,"data":…}, which
		// every OCS client reads as a malformed response.
		_ = json.NewEncoder(w).Encode(struct {
			OCS ocsEnvelope `json:"ocs"`
		}{OCS: env})
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(httpStatus)
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(env)
	_, _ = w.Write([]byte("\n"))
}

// wantsJSON reports whether the caller asked for JSON. Nextcloud selects by the
// "format" query parameter; the Accept header is honoured as well because that is what a
// generic HTTP client sends.
func wantsJSON(r *http.Request) bool {
	if strings.EqualFold(r.URL.Query().Get("format"), "json") {
		return true
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json")
}
