package server

// Node paths and content paths are not the same string, and conflating them is a data-access bug
// rather than a cosmetic one.
//
//   - A NODE PATH is what the API renders and accepts back: "/docs/report.txt". toFileJSON builds it
//     with a leading slash, the file manager passes the value back verbatim to download, preview,
//     thumbnail, upload, rename, move and archive.
//   - A CONTENT PATH is what a ContentStore is addressed by: "docs/report.txt", relative to the
//     owner's root, no leading slash. The tree layout refuses an absolute path outright
//     (layout.ErrUnsafePath) and that refusal is defence, not a limitation to be relaxed.
//
// Every handler used to hand the node path straight to the content store. The blob layout resolves
// it either way (paths are metadata there), so the mismatch survived a green suite — until the same
// request ran on the tree layout, where the product's own listing produced a URL its own download
// endpoint answered with 400 invalid_name. Normal download, inline preview, thumbnails and uploads
// into a folder were all affected (2026-10-23 review, P0).
//
// The conversion therefore happens HERE, once, from the RESOLVED metadata identity — not by trimming
// the caller's string. A name that a bug put a slash in, or a caller that sent "//a/./b", cannot
// change which node's bytes are opened, because the path is rebuilt from the nodes the caller was
// authorized for. The storage layer keeps refusing absolute and traversing paths; this layer is what
// produces the form it accepts.

import (
	"strings"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// relOfChain renders the content path of a resolved node chain.
//
// nodes comes from Store.Resolve, root first. Node names are validated at creation
// (layout.ValidateSegment), so joining them yields a relative path with no leading slash, no empty
// segment and no traversal element by construction — the two representations cannot drift apart,
// because this is the node's identity rather than the caller's text.
func relOfChain(nodes []store.Node) string {
	if len(nodes) <= 1 {
		// The zero case is the caller's root: a content path of "", which every layout addresses.
		return ""
	}
	names := make([]string, 0, len(nodes)-1)
	for _, n := range nodes[1:] {
		names = append(names, n.Name)
	}
	return strings.Join(names, "/")
}
