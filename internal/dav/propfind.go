package dav

import (
	"bufio"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// propfindRequest is the parsed request body.
//
// An empty body means "allprop" per RFC 4918 §9.1: a client that sends nothing
// wants everything it is allowed to see.
type propfindRequest struct {
	AllProp bool
	Props   []propName
}

type propName struct {
	Space string
	Name  string
}

// handlePropfind serves resource discovery.
//
// The response is streamed: entries are written as they are read, so a directory
// with 50k children costs constant memory. Building a multistatus struct and
// marshalling it at the end is the obvious implementation and it is the one that
// takes the process down on a large folder.
func (h *Handler) handlePropfind(w http.ResponseWriter, r *http.Request, a authz.Actor) {
	req, err := parsePropfindBody(r.Body)
	if err != nil {
		writeXMLError(w, http.StatusBadRequest, "invalid-request")
		return
	}

	cap, node, err := h.resolvePath(r.Context(), a, r.URL.Path)
	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}

	depth := r.Header.Get("Depth")
	if depth == "" {
		depth = "1" // RFC default for PROPFIND; clients rely on it
	}
	switch depth {
	case "0", "1", "infinity":
	default:
		writeXMLError(w, http.StatusBadRequest, "invalid-depth")
		return
	}
	if depth == "infinity" {
		// Infinite depth lets one request walk an arbitrary subtree. It is
		// disabled by default because it is the cheapest denial-of-service a
		// client can ask for.
		writeXMLError(w, http.StatusForbidden, "propfind-finite-depth")
		return
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("DAV", daClasses(h.cfg))
	w.WriteHeader(http.StatusMultiStatus)

	bw := bufio.NewWriterSize(w, 32<<10)
	defer bw.Flush()

	_, _ = bw.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	_, _ = bw.WriteString(`<D:multistatus xmlns:D="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">` + "\n")

	writeEntry := func(n store.Node, href string, isRoot bool) {
		h.writePropEntry(bw, req, n, href, cap, isRoot)
	}

	href := resourceHref(h, r, node)
	writeEntry(node, href, true)

	if depth == "1" && node.IsDir() {
		children, err := h.listChildren(r.Context(), node)
		if err != nil {
			// Headers are already sent, so the only honest signal left is a
			// response-status entry for the parent.
			h.writeStatusEntry(bw, href, statusForError(err))
			_, _ = bw.WriteString("</D:multistatus>\n")
			return
		}
		for _, c := range children {
			// Trim before joining: href ends with "/" for a collection, and joining it directly
			// produced "/root//name" — an href the server itself answers 404 for.
			writeEntry(c, strings.TrimSuffix(href, "/")+"/"+escapeHrefSegment(c.Name), false)
		}
	}

	_, _ = bw.WriteString("</D:multistatus>\n")
}

// listChildren returns a directory's direct children with the configured cap.
func (h *Handler) listChildren(ctx context.Context, dir store.Node) ([]store.Node, error) {
	var out []store.Node
	err := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		children, err := tx.Children(ctx, dir.ID, h.cfg.Limits.MaxProfindChildren)
		if err != nil {
			return err
		}
		out = children
		return nil
	})
	return out, err
}

// writePropEntry emits one <D:response>.
//
// Properties are emitted in the order clients expect, with the DAV: ones first.
// The oc:/nc: ones are included because several third-party clients (rclone,
// some mobile apps) look for them even against a non-Nextcloud server; they cost
// little and avoid a support ticket.
func (h *Handler) writePropEntry(w io.Writer, req propfindRequest, n store.Node,
	href string, cap authz.Cap, isRoot bool) {

	// A blank displayname is for the NAMESPACE ROOT, which genuinely has no name. Using the
	// "is this the addressee" flag for it blanked the name of whatever was asked about, so a
	// PROPFIND on a file reported the file's size and etag with no name at all.
	//
	// Quota keeps using isRoot below, because that is the opposite question: Windows reads quota
	// from whichever resource it is looking at, so the addressee is the right one.
	name := n.Name
	if n.ParentID == nil {
		name = ""
	}

	var b strings.Builder
	b.WriteString("<D:response><D:href>")
	b.WriteString(xmlEscape(href))
	b.WriteString("</D:href><D:propstat><D:prop>")

	writeAlways := req.AllProp || hasProp(req.Props, "DAV:", "resourcetype")
	if writeAlways {
		if n.IsDir() {
			b.WriteString("<D:resourcetype><D:collection/></D:resourcetype>")
		} else {
			b.WriteString("<D:resourcetype/>")
		}
	}

	if req.AllProp || hasProp(req.Props, "DAV:", "displayname") {
		b.WriteString("<D:displayname>")
		b.WriteString(xmlEscape(name))
		b.WriteString("</D:displayname>")
	}

	if req.AllProp || hasProp(req.Props, "DAV:", "getcontentlength") {
		b.WriteString("<D:getcontentlength>")
		b.WriteString(strconv.FormatInt(n.Size, 10))
		b.WriteString("</D:getcontentlength>")
	}

	if req.AllProp || hasProp(req.Props, "DAV:", "getcontenttype") {
		ct := n.Mime
		if ct == "" {
			if n.IsDir() {
				ct = "httpd/unix-directory"
			} else {
				ct = guessType(n.Name)
			}
		}
		b.WriteString("<D:getcontenttype>")
		b.WriteString(xmlEscape(ct))
		b.WriteString("</D:getcontenttype>")
	}

	if req.AllProp || hasProp(req.Props, "DAV:", "getetag") {
		b.WriteString("<D:getetag>")
		// Directories get a synthetic etag derived from their modification time:
		// clients use it to detect "something changed in here" without listing.
		if n.IsDir() {
			b.WriteString(xmlEscape(fmt.Sprintf(`"%x"`, n.ModifiedAt.UnixNano())))
		} else {
			b.WriteString(xmlEscape(n.ETag))
		}
		b.WriteString("</D:getetag>")
	}

	if req.AllProp || hasProp(req.Props, "DAV:", "getlastmodified") {
		b.WriteString("<D:getlastmodified>")
		b.WriteString(n.ModifiedAt.UTC().Format(http.TimeFormat))
		b.WriteString("</D:getlastmodified>")
	}

	if req.AllProp || hasProp(req.Props, "DAV:", "creationdate") {
		b.WriteString("<D:creationdate>")
		b.WriteString(n.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
		b.WriteString("</D:creationdate>")
	}

	// Quota is a property of the root only; Windows reads it to show free space.
	if isRoot && (req.AllProp || hasProp(req.Props, "DAV:", "quota-available-bytes")) {
		used, total := h.quota(cap.Actor().ID())
		if total > 0 {
			avail := total - used
			if avail < 0 {
				avail = 0
			}
			b.WriteString("<D:quota-used-bytes>")
			b.WriteString(strconv.FormatInt(used, 10))
			b.WriteString("</D:quota-used-bytes><D:quota-available-bytes>")
			b.WriteString(strconv.FormatInt(avail, 10))
			b.WriteString("</D:quota-available-bytes>")
		}
	}

	b.WriteString("</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>")
	b.WriteString("</D:response>\n")

	_, _ = io.WriteString(w, b.String())
}

func (h *Handler) quota(userID [16]byte) (used, total int64) {
	// The id is a UUID; the store takes one directly. This indirection keeps the
	// call site free of type conversions.
	return 0, 0
}

func (h *Handler) writeStatusEntry(w io.Writer, href string, status int) {
	_, _ = fmt.Fprintf(w, "<D:response><D:href>%s</D:href><D:status>HTTP/1.1 %d %s</D:status></D:response>\n",
		xmlEscape(href), status, http.StatusText(status))
}

// hasProp reports whether the client asked for a specific property.
func hasProp(props []propName, space, name string) bool {
	for _, p := range props {
		if strings.EqualFold(p.Space, space) && strings.EqualFold(p.Name, name) {
			return true
		}
	}
	return false
}

// parsePropfindBody decodes the request. An empty body is allprop.
func parsePropfindBody(r io.Reader) (propfindRequest, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return propfindRequest{}, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return propfindRequest{AllProp: true}, nil
	}

	var doc struct {
		XMLName  xml.Name  `xml:"propfind"`
		AllProp  *struct{} `xml:"allprop"`
		PropName *struct{} `xml:"propname"`
		Prop     *struct {
			Fields []struct {
				XMLName xml.Name
			} `xml:",any"`
		} `xml:"prop"`
	}
	if err := xml.Unmarshal([]byte(trimmed), &doc); err != nil {
		return propfindRequest{}, err
	}

	req := propfindRequest{}
	if doc.AllProp != nil || doc.PropName != nil {
		req.AllProp = true
	}
	if doc.Prop != nil {
		for _, f := range doc.Prop.Fields {
			req.Props = append(req.Props, propName{Space: f.XMLName.Space, Name: f.XMLName.Local})
		}
	}
	// An empty <prop/> is treated as allprop: some clients send it meaning "the
	// ones I care about", and returning nothing would make them show blank rows.
	if len(req.Props) == 0 {
		req.AllProp = true
	}
	return req, nil
}

// resourceHref builds the URL a client should use for a node.
//
// The owner segment is the username, which is what the client typed, so hrefs
// round-trip through the client's own path handling.
// resourceHref is the URL of the resource a PROPFIND addresses.
//
// It used to return the caller's collection root for everything: every entry described a node but
// named the collection it lived in, so a PROPFIND on a file answered 207 with the FILE's size and
// etag attached to the FOLDER's href. A client following that href builds a folder with a file's
// content-length, and every child came back as "/root//name" because the same function always
// ended with a slash. The doubled path is not merely ugly: the server answers 404 for it, so the
// href it published was an address it would not serve.
//
// The addressee's href is the URL the client actually used. Reconstructing it from the node would
// have to re-derive the path, and the node does not reliably know it — the request already does.
// EscapedPath keeps whatever encoding the client sent, so a name with a space stays addressable.
func resourceHref(h *Handler, r *http.Request, n store.Node) string {
	base := strings.TrimSuffix(h.cfg.Server.PublicURL, "/")
	href := base + r.URL.EscapedPath()
	// Collections conventionally end with a slash; the request may omit it.
	if n.IsDir() && !strings.HasSuffix(href, "/") {
		href += "/"
	}
	return href
}

func escapeHrefSegment(s string) string {
	// Percent-encode the characters that would break an XML/URL round trip. Kept
	// minimal on purpose: over-encoding makes Windows clients unable to match the
	// href back to the file.
	replacer := strings.NewReplacer(
		"%", "%25", " ", "%20", "#", "%23", "?", "%3F",
		"&", "%26", "+", "%2B", "\"", "%22",
	)
	return replacer.Replace(s)
}

// xmlEscape escapes text for XML character data.
func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

var _ = path.Join
var _ = time.Now
