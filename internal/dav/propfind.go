package dav

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// ownCloudNS is the ownCloud/Nextcloud property namespace.
//
// The checksum, favorite and tag properties live here because that is where the ecosystem's clients
// look for them (core-scope §2: "the ownCloud/Nextcloud property, so third-party clients find it").
// A property this server invents in its own namespace would be discoverable only by a client
// written for it, which defeats the interoperability reason for exposing them at all.
const ownCloudNS = "http://owncloud.org/ns"

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
	// Depth tokens compare case-insensitively (RFC 4918 §9.1 spells it "Infinity"); the
	// rest of the handler keeps the canonical lowercase form (2026-10-25 review).
	switch strings.ToLower(depth) {
	case "0", "1", "infinity":
		depth = strings.ToLower(depth)
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

	// Children are read BEFORE the response starts, and the dead properties of the whole
	// listing are read in ONE query.
	//
	// The Nextcloud clients request about twenty non-DAV properties on every entry, so the
	// old per-entry lookup opened a transaction per child: a 500-entry folder was 500 round
	// trips before the client saw a complete response. Reading first also lets a listing
	// failure be an error response rather than a half-written multistatus.
	var children []store.Node
	if depth == "1" && node.IsDir() {
		list, err := h.listChildren(r.Context(), node)
		if err != nil {
			writeXMLError(w, statusForError(err), "")
			return
		}
		children = list
	}

	// One query for every live lock at or below the address, indexed by node.
	//
	// lockdiscovery is a property of every entry, and asking the store per entry would be N+1
	// on a folder with a thousand children. LocksInSubtree already exists for the DELETE fence
	// and answers exactly this shape.
	locks := map[uuid.UUID][]store.Lock{}
	if lerr := h.meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		held, err := tx.LocksInSubtree(ctx, node.ID)
		if err != nil {
			return err
		}
		for _, l := range held {
			locks[l.NodeID] = append(locks[l.NodeID], l)
		}
		return nil
	}); lerr != nil {
		// A listing is not worth failing over lock state, and the property then says "no locks",
		// which is wrong in the safe direction: a client that tries to write is still refused by
		// the write path, which checks the store directly.
		h.logf("propfind: could not read locks for %q: %v", r.URL.Path, lerr)
	}

	// The same trick for dead properties: only when the request could ask for one, and once
	// for the whole listing.
	deadByNode := map[uuid.UUID][]store.DeadProp{}
	if h.propfindWantsDead(req) {
		ids := make([]uuid.UUID, 0, len(children)+1)
		ids = append(ids, node.ID)
		for _, c := range children {
			ids = append(ids, c.ID)
		}
		if props, derr := h.deadPropsOfMany(r.Context(), ids); derr == nil {
			deadByNode = props
		} else {
			h.logf("propfind: dead properties unavailable for %q: %v", r.URL.Path, derr)
		}
	}

	// One batch for the live social properties (core-scope §2, §4, §5): the checksum, the viewer's
	// star and the node's tags. Asking per entry would be three queries per child on a listing of a
	// thousand, which is the N+1 the dead-property batch above exists to avoid.
	socialByNode := map[uuid.UUID]store.NodeSocial{}
	if h.propfindWantsSocial(req) {
		ids := make([]uuid.UUID, 0, len(children)+1)
		ids = append(ids, node.ID)
		for _, c := range children {
			ids = append(ids, c.ID)
		}
		if social, serr := h.socialOfMany(r.Context(), cap.Actor().ID(), ids); serr == nil {
			socialByNode = social
		} else {
			// A cosmetic loss, not a failed listing: a client that asked for the checksum gets it
			// in the 404 propstat, which is the RFC-correct answer for "this server cannot say".
			h.logf("propfind: social properties unavailable for %q: %v", r.URL.Path, serr)
		}
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("DAV", daClasses(h.cfg))
	w.WriteHeader(http.StatusMultiStatus)

	bw := bufio.NewWriterSize(w, 32<<10)
	defer bw.Flush()

	_, _ = bw.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	_, _ = bw.WriteString(`<D:multistatus xmlns:D="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">` + "\n")

	writeEntry := func(n store.Node, href string, isRoot bool) {
		h.writePropEntry(bw, req, n, href, cap, isRoot, locks, deadByNode[n.ID], socialByNode[n.ID])
	}

	href := resourceHref(r, node)
	writeEntry(node, href, true)

	for _, c := range children {
		// Trim before joining: href ends with "/" for a collection, and joining it directly
		// produced "/root//name" — an href the server itself answers 404 for.
		writeEntry(c, strings.TrimSuffix(href, "/")+"/"+escapeHrefSegment(c.Name), false)
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
//
// dead is this entry's dead properties, fetched in one query for the whole listing before
// the response started (see deadPropsOfMany). It used to be fetched here, per entry, which
// turned a large listing into one transaction per child.
func (h *Handler) writePropEntry(w io.Writer, req propfindRequest, n store.Node,
	href string, cap authz.Cap, isRoot bool, locks map[uuid.UUID][]store.Lock,
	dead []store.DeadProp, social store.NodeSocial) {

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
		// A directory's etag is its CHANGE TOKEN (M1): every mutation under it stamps a
		// fresh one on it and every directory above it, so a walking sync client compares
		// the token and skips a subtree it already listed — that walk is what makes generic
		// WebDAV sync slow, and this is the server's half of pruning it.
		//
		// The mtime fallback serves rows written before tokens existed: they carry an empty
		// etag until the next mutation under them stamps one, and answering them with no etag
		// at all would be a regression for clients mid-upgrade.
		if n.IsDir() {
			if n.ETag != "" {
				b.WriteString(xmlEscape(n.ETag))
			} else {
				b.WriteString(xmlEscape(fmt.Sprintf(`"%x"`, n.ModifiedAt.UnixNano())))
			}
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

	// Class 2 requires both of these (RFC 4918 §6.2, §15).
	//
	// supportedlock is what lets a client know a lock is even possible; without it Windows and
	// Office fall back to their least informative error. lockdiscovery reports the locks
	// actually held, and an empty element is the correct answer for "none" — omitting the
	// property instead makes "not locked" indistinguishable from "server does not support
	// locks", which is the state this server was in before the 2026-10-24 review.
	if req.AllProp || hasProp(req.Props, "DAV:", "supportedlock") {
		b.WriteString("<D:supportedlock><D:lockentry>" +
			"<D:lockscope><D:exclusive/></D:lockscope>" +
			"<D:locktype><D:write/></D:locktype></D:lockentry></D:supportedlock>")
	}
	if req.AllProp || hasProp(req.Props, "DAV:", "lockdiscovery") {
		b.WriteString("<D:lockdiscovery>")
		for _, l := range locks[n.ID] {
			b.WriteString(activeLockXML(l, href))
		}
		b.WriteString("</D:lockdiscovery>")
	}

	// The live social properties (core-scope §2, §4, §5), in the ownCloud/Nextcloud namespaces so
	// third-party clients find them without a mapping table.
	//
	//   oc:checksums   "SHA256:<hex>" — the ownCloud property. The blob address IS the content hash,
	//                  so this is surface plumbing rather than new hashing, and it is the reason the
	//                  hash is computed identically on both layouts.
	//   oc:favorite    "0"/"1" — a LIVE property, not a dead one: it is per viewer and derived from
	//                  the favorites table, so storing it as a dead property would make it a
	//                  per-NODE flag, which is exactly what §4 says it must not be.
	//   oc:tags        space-separated names — read-only here. Assignment stays JSON because it is
	//                  a metadata write with its own permission rule, and a PROPPATCH that silently
	//                  rewrote somebody's labels would be a write path with a weaker check.
	if req.AllProp || hasProp(req.Props, ownCloudNS, "checksums") {
		// A directory has no content, so the property is omitted and lands in the 404 propstat.
		if !n.IsDir() && len(social.SHA256) == sha256.Size {
			b.WriteString("<oc:checksums><oc:checksum>SHA256:")
			b.WriteString(hex.EncodeToString(social.SHA256))
			b.WriteString("</oc:checksum></oc:checksums>")
		}
	}
	if req.AllProp || hasProp(req.Props, ownCloudNS, "favorite") {
		b.WriteString("<oc:favorite>")
		if social.Starred {
			b.WriteString("1")
		} else {
			b.WriteString("0")
		}
		b.WriteString("</oc:favorite>")
	}
	if req.AllProp || hasProp(req.Props, ownCloudNS, "tags") {
		// An empty element is the correct answer for "no tags": omitting it makes "none" and "this
		// server cannot say" indistinguishable, which is the state the dead-property emitter was in
		// before it was fixed.
		b.WriteString("<oc:tags>")
		b.WriteString(xmlEscape(tagNames(social.Tags)))
		b.WriteString("</oc:tags>")
	}

	// Dead properties come back, because otherwise PROPPATCH is write-only.
	//
	// The store has kept them (Windows Explorer and macOS Finder write per-file metadata there
	// and misbehave when it does not survive a reconnect), but PROPFIND never returned one, so
	// a client that set a property and re-read it saw nothing and concluded the server had
	// ignored it. They are fetched in one batch by the caller, only when the request could
	// care; a plain live-property listing pays nothing.
	for _, p := range dead {
		b.WriteString("<")
		b.WriteString(xmlEscape(p.Name))
		b.WriteString(` xmlns="`)
		b.WriteString(xmlEscape(p.Space))
		b.WriteString(`">`)
		b.WriteString(xmlEscape(p.Value))
		b.WriteString("</")
		b.WriteString(xmlEscape(p.Name))
		b.WriteString(">")
	}

	b.WriteString("</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>")

	// Every property the client named must be answered in one propstat or the other
	// (RFC 4918 §9.1). Omitting the ones this server does not have left the client unable to
	// tell "not set" from "the server ignored that part of the request".
	if !req.AllProp {
		var missing []propName
		for _, p := range req.Props {
			if !h.propIsEmitted(p, dead) {
				missing = append(missing, p)
			}
		}
		if len(missing) > 0 {
			b.WriteString("<D:propstat><D:prop>")
			for _, p := range missing {
				b.WriteString(emptyPropXML(p))
			}
			b.WriteString("</D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>")
		}
	}

	b.WriteString("</D:response>\n")

	_, _ = io.WriteString(w, b.String())
}

// propIsEmitted reports whether this handler answers a named property with a value.
//
// It must match the branches above exactly: a property listed here but not emitted disappears
// from the response entirely, which is the failure this function exists to prevent. A non-DAV
// property is only emitted when the node actually HAS it, so "never set" lands in the 404
// bucket where a client can see it.
func (h *Handler) propIsEmitted(p propName, dead []store.DeadProp) bool {
	// The live social properties are answered in the ownCloud namespace (core-scope §2, §4, §5).
	//
	// oc:checksums is claimed only when a value can be produced — a directory, or a file whose
	// address is unknown, answers in the 404 propstat. oc:favorite and oc:tags are always answered
	// because "0" and "" are meaningful values, and a client that asked for them must not see them
	// vanish.
	if strings.EqualFold(p.Space, ownCloudNS) {
		switch strings.ToLower(p.Name) {
		case "favorite", "tags":
			return true
		case "checksums":
			// Claimed unconditionally here and emitted only when known; the caller's 404 propstat is
			// what tells a client the value is unavailable, and a property that is sometimes listed
			// and sometimes not is worse than one that is consistently answered.
			return true
		}
	}
	if !strings.EqualFold(p.Space, "DAV:") {
		for _, d := range dead {
			if strings.EqualFold(d.Name, p.Name) && strings.EqualFold(d.Space, p.Space) {
				return true
			}
		}
		return false
	}
	switch p.Name {
	case "resourcetype", "displayname", "getcontentlength", "getcontenttype", "getetag",
		"getlastmodified", "creationdate", "supportedlock", "lockdiscovery":
		return true
	}
	// quota-used-bytes / quota-available-bytes are deliberately NOT claimed: quota() is still
	// a stub, so the emitter never writes them, and claiming them made a requested quota
	// property disappear from the response entirely instead of being answered in the 404
	// propstat, which is the RFC-correct answer until quota reporting is wired. Wiring it
	// needs per-owner usage plus a configured quota-byte figure (Store.Usage / SetQuota), at
	// which point both names come back here (2026-10-25 review).
	return false
}

// propfindWantsDead reports whether a request could be asking for dead properties.
func (h *Handler) propfindWantsDead(req propfindRequest) bool {
	if req.AllProp {
		return true
	}
	for _, p := range req.Props {
		// The live social properties are NOT dead properties: they are answered from the metadata
		// batch, so a request that names one of them must not trigger the dead-property query.
		if strings.EqualFold(p.Space, ownCloudNS) && isLiveOwnCloudProp(p.Name) {
			continue
		}
		if !strings.EqualFold(p.Space, "DAV:") {
			return true
		}
	}
	return false
}

// propfindWantsSocial reports whether a request could be asking for the live social properties.
//
// allprop asks for everything, and the three social properties are part of "everything a client may
// see" — so they are fetched for a plain allprop too. They are one batch query, so the cost is a
// constant, not a per-entry lookup.
func (h *Handler) propfindWantsSocial(req propfindRequest) bool {
	if req.AllProp {
		return true
	}
	for _, p := range req.Props {
		if strings.EqualFold(p.Space, ownCloudNS) && isLiveOwnCloudProp(p.Name) {
			return true
		}
	}
	return false
}

// isLiveOwnCloudProp reports whether a property in the ownCloud namespace is answered from metadata
// rather than from the dead-property table.
func isLiveOwnCloudProp(name string) bool {
	switch strings.ToLower(name) {
	case "checksums", "favorite", "tags":
		return true
	}
	return false
}

// socialOfMany reads the live social properties of several nodes in one batch.
func (h *Handler) socialOfMany(ctx context.Context, viewer uuid.UUID,
	nodeIDs []uuid.UUID) (map[uuid.UUID]store.NodeSocial, error) {

	reader, ok := h.meta.(store.SocialReader)
	if !ok {
		// A backend without the capability answers "cannot say" for every entry, which is what the
		// empty map produces. Losing a decoration must never fail a listing.
		return map[uuid.UUID]store.NodeSocial{}, nil
	}
	return reader.SocialFor(ctx, viewer, nodeIDs)
}

// tagNames renders a tag list as the space-separated form oc:tags uses.
//
// A tag name may contain a space (nothing forbids it), so a space-separated value is lossy in
// principle. It is the format Nextcloud clients parse, and the lossy case is a tag with a space in
// its name — which a client then shows as two tags. That is stated rather than hidden: the JSON API
// is the authoritative surface for tags, and oc:tags is the compatibility read.
func tagNames(tags []store.Tag) string {
	names := make([]string, 0, len(tags))
	for _, t := range tags {
		names = append(names, t.Name)
	}
	return strings.Join(names, " ")
}

// deadPropsOfMany reads the dead properties of several nodes in one query, for a listing.
func (h *Handler) deadPropsOfMany(ctx context.Context, nodeIDs []uuid.UUID) (map[uuid.UUID][]store.DeadProp, error) {
	var out map[uuid.UUID][]store.DeadProp
	err := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		props, err := tx.DeadPropsFor(ctx, nodeIDs)
		out = props
		return err
	})
	return out, err
}

// activeLockXML renders one <D:activelock> for lockdiscovery.
//
// href is the entry's own URL and becomes the lockroot: §14.2 makes lockroot part of an
// activelock, and a client matches the lock to the resource it edited by comparing it. The
// lock's submitted owner is echoed too — escaped, never replayed as markup — which is how a
// co-authoring client shows "locked by" (2026-10-25 review).
func activeLockXML(l store.Lock, href string) string {
	scope := "<D:exclusive/>"
	depth := l.Depth
	if depth == "" {
		depth = "infinity"
	}
	owner := ""
	if strings.TrimSpace(l.OwnerXML) != "" {
		owner = "<D:owner>" + xmlEscape(l.OwnerXML) + "</D:owner>"
	}
	secs := int(time.Until(l.ExpiresAt).Seconds())
	if secs < 0 {
		secs = 0
	}
	// Element order follows §14.2's activelock sequence: locktype, lockscope, depth, owner?,
	// timeout?, locktoken?, lockroot.
	return fmt.Sprintf("<D:activelock><D:locktype><D:write/></D:locktype>"+
		"<D:lockscope>%s</D:lockscope><D:depth>%s</D:depth>%s"+
		"<D:timeout>Second-%d</D:timeout><D:locktoken><D:href>%s</D:href></D:locktoken>"+
		"<D:lockroot><D:href>%s</D:href></D:lockroot>",
		scope, xmlEscape(depth), owner, secs, xmlEscape(l.Token), xmlEscape(href))
}

// emptyPropXML renders a property element with no value, for the 404 propstat.
//
// DAV: names reuse the document's D: prefix; anything else gets a local declaration, because
// the multistatus cannot know which prefixes a client will use and a wrong prefix would make
// the response unparseable rather than merely empty.
func emptyPropXML(p propName) string {
	if strings.EqualFold(p.Space, "DAV:") {
		return "<D:" + xmlEscape(p.Name) + "/>"
	}
	return `<X:` + xmlEscape(p.Name) + ` xmlns:X="` + xmlEscape(p.Space) + `"/>`
}

func (h *Handler) quota(userID [16]byte) (used, total int64) {
	// The id is a UUID; the store takes one directly. This indirection keeps the
	// call site free of type conversions.
	return 0, 0
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
// It is an ABSOLUTE PATH, not a full URL, and that is load-bearing for the Nextcloud
// clients: their discovery parser compares every href with the request URL's path
// (`QUrl::path()` of what they asked for) and drops the response as invalid when an href
// does not start with it. Nextcloud itself answers with paths for the same reason. A path
// is also the more robust answer behind a reverse proxy, where the public URL and the Host
// the client used can differ.
//
// The addressee's href is the path the client actually used, in the encoding it sent it.
// Reconstructing it from the node would have to re-derive the path, and the node does not
// reliably know it — the request already does. EscapedPath keeps whatever encoding the
// client used, so a name with a space stays addressable.
func resourceHref(r *http.Request, n store.Node) string {
	href := r.URL.EscapedPath()
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
