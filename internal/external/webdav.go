package external

// The WebDAV provider: the remote is an HTTP server speaking RFC 4918, and every
// filesystem operation is one round trip the protocol already defines.
//
// This is deliberately a client of the protocol, not of any server: it issues plain
// PROPFIND/GET/PUT/MKCOL/DELETE/MOVE and accepts any 207/2xx that answers. The test for
// it mounts this provider against this project's OWN WebDAV handler, which is both the
// cheapest real server available and a dogfooding check — a client and server written in
// the same repository that cannot talk to each other is a finding about one of them.

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WebDAV is a Storage over an HTTP WebDAV server.
type WebDAV struct {
	base   *url.URL // endpoint + rootPath, no trailing slash
	user   string
	pass   string
	client *http.Client
}

// DialWebDAV builds the provider. endpoint is the collection URL (the server's DAV root
// plus any prefix); rootPath narrows the mounted subtree below it and may be empty.
func DialWebDAV(ctx context.Context, endpoint, rootPath, username, password string) (Storage, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("webdav: bad endpoint: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("webdav: endpoint must be http(s), got %q", u.Scheme)
	}
	if r := strings.Trim(rootPath, "/"); r != "" {
		u.Path = path.Join(u.Path, r)
	}
	return &WebDAV{
		base:   u,
		user:   username,
		pass:   password,
		client: &http.Client{}, // per-request contexts bound every call
	}, nil
}

// urlFor joins the base with a clean relative path, percent-encoding each segment so a
// remote file named "a b#c.txt" is one path segment, not a query and a fragment.
func (w *WebDAV) urlFor(rel string) string {
	segs := splitSegments(rel)
	parts := make([]string, len(segs))
	for i, s := range segs {
		parts[i] = url.PathEscape(s)
	}
	joined := strings.Join(parts, "/")
	if joined == "" {
		return w.base.String() + "/"
	}
	return w.base.String() + "/" + joined
}

func splitSegments(rel string) []string {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." {
			continue
		}
		out = append(out, part)
	}
	return out
}

func (w *WebDAV) do(ctx context.Context, method, urlStr string, body io.Reader, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return nil, err
	}
	if w.user != "" || w.pass != "" {
		req.SetBasicAuth(w.user, w.pass)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return w.client.Do(req)
}

// mapStatus turns a WebDAV/HTTP status into the provider error vocabulary. Everything
// unmapped is unavailable: an answer this client cannot act on is an outage, not a guess.
func mapStatus(code int, body io.Reader) error {
	switch {
	case code == http.StatusNotFound:
		return ErrNotFound
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ErrPermission
	case code == http.StatusMethodNotAllowed || code == http.StatusConflict || code == http.StatusPreconditionFailed:
		return ErrExists // MKCOL/DELETE/MOVE conflict shapes; the caller names the op
	case code >= 500:
		return ErrUnavailable
	default:
		snippet, _ := io.ReadAll(io.LimitReader(body, 200))
		return fmt.Errorf("%w: status %d: %s", ErrUnavailable, code, strings.TrimSpace(string(snippet)))
	}
}

// propfindResponse mirrors the multistatus body this client needs: href, collection-ness,
// size and modified time. Anything else in the response is ignored by the decoder.
type propfindResponse struct {
	XMLName xml.Name `xml:"multistatus"`
	Rows    []struct {
		Href string `xml:"href"`
		// One response can carry several propstats (found props, missing props); the
		// status on each decides which ones to read. Paths are relative to the
		// element they sit in — "response>href" inside a row bound to "response"
		// would never match.
		Props []struct {
			Status string `xml:"status"`
			// A bool field would parse the element's (empty) chardata, not its
			// PRESENCE — encoding/xml has no presence semantics. A slice is the
			// presence test: non-empty means <collection/> occurred.
			IsCollection  []xml.Name `xml:"prop>resourcetype>collection"`
			ContentLength string     `xml:"prop>getcontentlength"`
			LastModified  string     `xml:"prop>getlastmodified"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// propfind issues one PROPFIND. keepSelf governs the self-row filter: a listing drops it,
// a stat IS it.
func (w *WebDAV) propfind(ctx context.Context, rel string, depth string, keepSelf bool) ([]Entry, error) {
	resp, err := w.do(ctx, "PROPFIND", w.urlFor(rel), nil, map[string]string{
		"Depth": depth, "Content-Type": "application/xml",
		"Accept": "application/xml",
	})
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, mapStatus(resp.StatusCode, resp.Body)
	}
	var ms propfindResponse
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&ms); err != nil {
		return nil, fmt.Errorf("%w: unparseable multistatus: %v", ErrUnavailable, err)
	}

	// The self href (the resource asked for) must be dropped from a listing. Servers
	// disagree on href shape — absolute URL, absolute path, with or without trailing
	// slash — so each href is normalized to the path below THIS provider's base, and
	// self is the cleaned request path below the base. What survives is exactly the
	// children, whatever the server's href convention is.
	basePath := path.Clean("/" + w.base.Path)
	self := path.Clean(basePath + "/" + strings.Join(splitSegments(rel), "/"))
	hrefBelow := func(raw string) (string, bool) {
		h, err := url.PathUnescape(raw)
		if err != nil {
			return "", false
		}
		// An absolute URL href contributes only its path.
		if u, err := url.Parse(h); err == nil && u.IsAbs() {
			h = u.Path
		}
		clean := path.Clean("/" + h)
		if !strings.HasPrefix(clean, basePath+"/") && clean != basePath {
			return "", false // a different tree: not our child, ignore it
		}
		return clean, true
	}

	var out []Entry
	for _, row := range ms.Rows {
		clean, ok := hrefBelow(row.Href)
		if !ok {
			continue
		}
		if !keepSelf && (clean == self || clean == basePath) {
			continue
		}
		if keepSelf && clean != self {
			continue
		}
		name := path.Base(clean)
		if name == "/" || name == "." {
			continue
		}
		e := Entry{Name: name}
		for _, p := range row.Props {
			if !strings.Contains(p.Status, "200") {
				continue
			}
			e.IsDir = e.IsDir || len(p.IsCollection) > 0
			if n, err := strconv.ParseInt(p.ContentLength, 10, 64); err == nil {
				e.Size = n
			}
			if t, err := http.ParseTime(p.LastModified); err == nil {
				e.ModTime = t
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (w *WebDAV) List(ctx context.Context, dir string) ([]Entry, error) {
	return w.propfind(ctx, dir, "1", false)
}

func (w *WebDAV) Stat(ctx context.Context, rel string) (Entry, error) {
	entries, err := w.propfind(ctx, rel, "0", true)
	if err != nil {
		return Entry{}, err
	}
	if len(entries) == 0 {
		return Entry{}, ErrNotFound
	}
	return entries[0], nil
}

func (w *WebDAV) Open(ctx context.Context, rel string) (io.ReadCloser, error) {
	resp, err := w.do(ctx, http.MethodGet, w.urlFor(rel), nil, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		defer resp.Body.Close()
		return nil, mapStatus(resp.StatusCode, resp.Body)
	}
	return resp.Body, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (w *WebDAV) Create(ctx context.Context, rel string, r io.Reader) (int64, error) {
	cr := &countingReader{r: r}
	resp, err := w.do(ctx, http.MethodPut, w.urlFor(rel), cr, map[string]string{
		"Content-Type": "application/octet-stream",
	})
	if err != nil {
		return 0, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return 0, mapStatus(resp.StatusCode, resp.Body)
	}
	return cr.n, nil
}

func (w *WebDAV) Mkdir(ctx context.Context, rel string) error {
	resp, err := w.do(ctx, "MKCOL", w.urlFor(rel), nil, nil)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return mapStatus(resp.StatusCode, resp.Body)
}

func (w *WebDAV) Remove(ctx context.Context, rel string, recursive bool) error {
	// WebDAV DELETE is recursive by definition; recursive=false cannot be expressed, so
	// the caller that needs it Stats first — refusing here would break the one protocol
	// where remote delete is cheapest.
	resp, err := w.do(ctx, http.MethodDelete, w.urlFor(rel), nil, nil)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return mapStatus(resp.StatusCode, resp.Body)
}

func (w *WebDAV) Rename(ctx context.Context, oldRel, newRel string) error {
	dest := w.urlFor(newRel)
	resp, err := w.do(ctx, "MOVE", w.urlFor(oldRel), nil, map[string]string{
		"Destination": dest, "Overwrite": "F",
	})
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode == http.StatusPreconditionFailed {
		// Overwrite:F and the destination exists: the honest mapping for a rename.
		return ErrExists
	}
	return mapStatus(resp.StatusCode, resp.Body)
}

func (w *WebDAV) Ping(ctx context.Context) error {
	resp, err := w.do(ctx, "PROPFIND", w.base.String()+"/", nil, map[string]string{
		"Depth": "0", "Content-Type": "application/xml", "Accept": "application/xml",
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusMultiStatus || resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrPermission
	default:
		return mapStatus(resp.StatusCode, resp.Body)
	}
}

// compile-time assertion of the interface.
var _ Storage = (*WebDAV)(nil)

// ensure time stays referenced for ModTime parsing docs (http.ParseTime needs RFC1123).
var _ = time.RFC1123
