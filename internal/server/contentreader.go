package server

// Text extraction for the content index (core-scope §7.2).
//
// This lives in the server package because that is where the ContentStore is, and the worker
// deliberately depends on a ONE-METHOD reader rather than on the whole content layer: an indexer
// that could Put, Move or Delete would be one bug away from rewriting a user's file with the text it
// was supposed to be reading.
//
// What is extracted, and why the list is short:
//
//	text/*, .md, .markdown    the bytes AS TEXT, which is what they are
//	application/pdf           the text streams, lifted by a minimal parser below
//
// Office formats are NOT here. They need an extractor service, and §7.2 makes that a separate
// decision rather than a silent gap: a deployment that believes it can search .docx and cannot is
// worse than one that knows the limit.
//
// PDF extraction is deliberately minimal — no OCR, no encryption support, no font-dependent
// decoding. It lifts the text from uncompressed and Flate-compressed content streams, which is what
// the overwhelming majority of generated PDFs contain. A scanned document has no text to lift and is
// therefore not searchable by content, which is the honest outcome; OCR is a different product.
// A document this parser cannot read keeps its METADATA in the index, so it stays findable by name.

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// ContentReader implements worker.DocumentReader over the configured content store.
type ContentReader struct {
	content layout.ContentStore
	// pathOf resolves a node's content path, because the tree layout addresses bytes by path and
	// the blob layout by content address. The worker hands over a node, and this is what turns that
	// into the thing ContentStore.Open accepts.
	pathOf func(ctx context.Context, node store.Node) (string, error)
}

// NewContentReader builds the reader.
func NewContentReader(content layout.ContentStore,
	pathOf func(ctx context.Context, node store.Node) (string, error)) *ContentReader {
	return &ContentReader{content: content, pathOf: pathOf}
}

// ReadContent returns a node's searchable text, bounded by maxBytes.
func (r *ContentReader) ReadContent(ctx context.Context, node store.Node, maxBytes int64) (string, error) {
	if r.content == nil {
		return "", fmt.Errorf("no content store is wired")
	}
	if node.Kind != store.KindFile {
		return "", nil
	}
	path, err := r.pathOf(ctx, node)
	if err != nil {
		return "", err
	}

	// Open through the layout abstraction rather than the filesystem: the two layouts differ in how
	// bytes are addressed, and an extractor that assumed one of them would silently index nothing on
	// the other.
	//
	// Owner + Path, not Hash: both shipped layouts resolve a node through the metadata by that pair
	// (the blob layout's Open asks its resolver for the address, the tree layout builds the path),
	// so this one call is correct on both. Setting a fabricated Hash here would be worse than
	// leaving it zero — it would look like an address and be one nobody computed.
	rc, err := r.content.Open(ctx, layout.Node{
		Owner: node.OwnerID.String(), Path: path, Size: node.Size,
	})
	if err != nil {
		return "", err
	}
	defer rc.Close()

	limit := maxBytes
	if limit <= 0 {
		limit = 10 << 20
	}
	// One byte over the cap is enough to know the body ran past it: reading it all and then
	// discarding the excess is the allocation this bound exists to prevent.
	raw, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(raw)) > limit {
		raw = raw[:limit]
	}

	if isPDF(node.Mime, node.Name) {
		return extractPDFText(raw), nil
	}
	return normaliseText(raw), nil
}

// PathOfNodeForIndex is the content-path resolver the indexer needs.
//
// It is exported because the worker lives in another package and the resolution has to be the
// SERVER's: the content path is derived from the metadata by the same walk the download path uses,
// and a second implementation in the worker would be a second answer to "where are this node's
// bytes" — which is exactly the class of divergence that made an earlier version index nothing on
// the tree layout.
//
// The store is a parameter rather than a Server field so the worker's own wiring can call it
// without constructing a Server.
func PathOfNodeForIndex(meta store.Store) func(ctx context.Context, node store.Node) (string, error) {
	return func(ctx context.Context, node store.Node) (string, error) {
		var chain []store.Node
		cur := node
		for depth := 0; depth < 256; depth++ {
			chain = append([]store.Node{cur}, chain...)
			if cur.ParentID == nil {
				break
			}
			var parent store.Node
			if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				got, gerr := tx.NodeIncludingTrashed(ctx, *cur.ParentID)
				parent = got
				return gerr
			}); err != nil {
				return "", err
			}
			cur = parent
		}
		// The owner's root has an empty name; including it would produce a leading slash, which the
		// tree layout refuses outright.
		segs := make([]string, 0, len(chain))
		for _, n := range chain {
			if n.Name == "" {
				continue
			}
			segs = append(segs, n.Name)
		}
		return strings.Join(segs, "/"), nil
	}
}

// isPDF reports whether a document is a PDF.
func isPDF(mime, name string) bool {
	if strings.HasPrefix(strings.ToLower(mime), "application/pdf") {
		return true
	}
	return strings.HasSuffix(strings.ToLower(name), ".pdf")
}

// normaliseText makes raw bytes into indexable text.
//
// Invalid UTF-8 is replaced rather than rejected: a mostly-text file with one bad byte is still
// worth searching, and refusing it would make the index quietly skip files for a reason nobody can
// see. NUL bytes are removed because PostgreSQL refuses them in a text column and Elasticsearch
// chokes on them in a JSON body — a "\u0000" in a document would fail the whole index write.
func normaliseText(raw []byte) string {
	raw = bytes.ReplaceAll(raw, []byte{0}, nil)
	if utf8.Valid(raw) {
		return string(raw)
	}
	return strings.ToValidUTF8(string(raw), "\uFFFD")
}

// extractPDFText lifts the text operators out of a PDF's content streams.
//
// A minimal parser, deliberately: it finds `stream ... endstream` blocks, inflates the ones that
// declare FlateDecode, and pulls the strings out of the text-showing operators (Tj, TJ, ', ").
// Everything else — fonts, encodings, positioning — is ignored, because the index needs the WORD
// CONTENT and not the layout.
//
// The outputs are approximate in a specific and acceptable way: a document whose text is stored as
// glyph indices rather than characters (CJK PDFs embedded with a subset font, typically) yields
// little or nothing. That is stated here rather than papered over, because a search that finds
// eight of ten documents and says nothing is exactly the "silently worse than promised" outcome
// §7.2 rules out.
func extractPDFText(raw []byte) string {
	var out strings.Builder
	rest := raw

	for {
		idx := bytes.Index(rest, []byte("stream"))
		if idx < 0 {
			break
		}
		// "endstream" contains "stream", so the search above can land inside one. Skip past the
		// token rather than looping on it.
		after := idx + len("stream")
		if after < len(rest) && rest[after] == 's' {
			rest = rest[after:]
			continue
		}
		// A stream's payload starts after the EOL that follows the keyword.
		body := rest[after:]
		body = bytes.TrimPrefix(body, []byte("\r\n"))
		body = bytes.TrimPrefix(body, []byte("\n"))
		body = bytes.TrimPrefix(body, []byte("\r"))

		end := bytes.Index(body, []byte("endstream"))
		if end < 0 {
			break
		}
		chunk := body[:end]
		rest = body[end+len("endstream"):]

		// Most generators compress content streams. A decoded chunk that looks like text is used
		// directly; an inflated one is where the operators live.
		if decoded, ok := inflate(chunk); ok {
			chunk = decoded
		}
		collectPDFStrings(chunk, &out)
	}
	return strings.TrimSpace(out.String())
}

// inflate decompresses a Flate-encoded stream, reporting whether it did.
func inflate(chunk []byte) ([]byte, bool) {
	r, err := zlib.NewReader(bytes.NewReader(chunk))
	if err != nil {
		return nil, false
	}
	defer r.Close()
	// A bound on the decompressed size: a small compressed stream can expand enormously, and an
	// indexer that can be made to allocate a gigabyte from a 4 KB upload is a denial of service.
	decoded, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil && len(decoded) == 0 {
		return nil, false
	}
	return decoded, true
}

// collectPDFStrings appends the strings inside text-showing operators.
func collectPDFStrings(chunk []byte, out *strings.Builder) {
	// A text-showing operator is a literal string `( ... )` (optionally inside an array) followed by
	// Tj, TJ, ' or ". Walking the parentheses and keeping every literal is a superset that also
	// catches strings in arrays, which is the common shape: [(Hel) -2 (lo)] TJ.
	for i := 0; i < len(chunk); i++ {
		if chunk[i] != '(' {
			continue
		}
		depth := 1
		var lit strings.Builder
		j := i + 1
		for ; j < len(chunk) && depth > 0; j++ {
			switch chunk[j] {
			case '\\':
				// An escape: the next byte is literal. The three-digit octal form is common for
				// accented characters and is handled explicitly.
				j++
				if j >= len(chunk) {
					break
				}
				if chunk[j] >= '0' && chunk[j] <= '7' && j+2 < len(chunk) {
					var v int
					for k := 0; k < 3 && j+k < len(chunk) && chunk[j+k] >= '0' &&
						chunk[j+k] <= '7'; k++ {
						v = v*8 + int(chunk[j+k]-'0')
					}
					lit.WriteByte(byte(v))
					j += 2
					continue
				}
				lit.WriteByte(chunk[j])
			case '(':
				depth++
				lit.WriteByte(chunk[j])
			case ')':
				depth--
				if depth > 0 {
					lit.WriteByte(chunk[j])
				}
			default:
				lit.WriteByte(chunk[j])
			}
		}
		if lit.Len() > 0 {
			text := normaliseText([]byte(lit.String()))
			// Control characters and single non-printable bytes are PDF structure rather than
			// words; a body made of them would pollute every query.
			if isMostlyPrintable(text) {
				out.WriteString(text)
				out.WriteString(" ")
			}
		}
		i = j - 1
	}
}

// isMostlyPrintable reports whether a extracted fragment looks like text.
func isMostlyPrintable(s string) bool {
	if s == "" {
		return false
	}
	printable := 0
	total := 0
	for _, r := range s {
		total++
		if r >= 0x20 && r != 0x7f {
			printable++
		}
	}
	return total > 0 && printable*10 >= total*8
}
