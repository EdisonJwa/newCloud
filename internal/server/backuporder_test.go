package server_test

// §11.2 item 4: "验证备份时间点附近的并发写入不会产生丢失引用."
//
// A backup here is two artefacts taken at two moments: a database dump, and a copy of the blob
// tree. They cannot be taken atomically, so there is always a window between them. Whether a write
// inside that window can produce a RESTORE THAT REFERENCES MISSING BYTES depends on the order, and
// which order is safe depends on an invariant of the write path:
//
//	the blob is written before its row.
//
// That is not an assumption — internal/server/files.go calls s.deps.Content.Put before
// publishFile, which inserts the row. TestBlobIsWrittenBeforeItsRow pins it.
//
// Given that invariant:
//
//	dump first, then copy blobs
//	    A write in the window has bytes in the copy but no row in the dump. The restored database
//	    never mentions the file, and the copy holds an unreferenced blob. Correct: the file is
//	    simply not in this backup, and the cost is some wasted space.
//
//	copy blobs first, then dump
//	    A write in the window has a row in the dump but its bytes arrived after the copy read that
//	    directory. The restored metadata references content that is not there, and every request
//	    for that file fails. The restore looks successful until someone opens it.
//
// So the safe order is DUMP FIRST, THEN BLOBS.
//
// This took two attempts, and the first version of this file asserted the opposite. The mistake was
// reasoning about "which artefact is newer" without checking which of the two writes happens first.
// Getting it backwards in the documentation would have been worse than not documenting it: it reads
// as advice, and an operator following it would produce exactly the broken restores the paragraph
// claims to prevent.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// captureBlobHashes records which content hashes are present under a local blob root, standing in
// for "the blob copy was taken now".
func captureBlobHashes(t *testing.T, root string) map[[32]byte]bool {
	t.Helper()
	out := map[[32]byte]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		hash, ok := parseHexHash(d.Name())
		if !ok {
			return nil
		}
		out[hash] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk the blob root %s: %v", root, err)
	}
	return out
}

// parseHexHash parses a 64-character lowercase hex file name into a content hash.
func parseHexHash(name string) ([32]byte, bool) {
	var out [32]byte
	if len(name) != 64 {
		return out, false
	}
	for i := 0; i < 32; i++ {
		var b byte
		for j := 0; j < 2; j++ {
			c := name[i*2+j]
			switch {
			case c >= '0' && c <= '9':
				b = b<<4 | (c - '0')
			case c >= 'a' && c <= 'f':
				b = b<<4 | (c - 'a' + 10)
			default:
				return out, false
			}
		}
		out[i] = b
	}
	return out, true
}

// captureFileRows records every file row visible to the harness account, standing in for "the
// dump was taken now". The hash comes from the row's blob id, which is the whole point: the
// restored metadata knows which content it needs, and the question is whether the copy has it.
func captureFileRows(t *testing.T, h *harness, root store.Node) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	var walk func(parent [16]byte)
	walk = func(parent [16]byte) {
		var children []store.Node
		if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.Children(ctx, parent, 10000)
			children = got
			return err
		}); err != nil {
			t.Fatalf("children: %v", err)
		}
		for _, c := range children {
			if c.IsDir() {
				walk(c.ID)
				continue
			}
			if c.BlobID == nil {
				continue
			}
			var raw []byte
			if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				got, err := tx.BlobHash(ctx, *c.BlobID)
				raw = got
				return err
			}); err != nil {
				t.Fatalf("blob hash for %s: %v", c.Name, err)
			}
			var hash [32]byte
			copy(hash[:], raw)
			out[c.Name] = hash
		}
	}
	walk(root.ID)
	return out
}

// TestDumpBeforeBlobsLosesNoReference is the safe order.
func TestDumpBeforeBlobsLosesNoReference(t *testing.T) {
	h := newHarness(t)
	root := h.rootNode(t)

	h.uploadFile(t, "stable.txt", "exists before the backup")

	// Artefact 1: the metadata dump.
	rowsInDump := captureFileRows(t, h, root)

	// A write inside the window.
	h.uploadFile(t, "during.txt", "written inside the backup window")

	// Artefact 2: the blob copy.
	blobsCopied := captureBlobHashes(t, h.blobRootOf(t))

	for name, hash := range rowsInDump {
		if !blobsCopied[hash] {
			t.Fatalf("the dump recorded %q needing blob %x, but the copy lacks it — the restored "+
				"system would report a missing blob for a file the backup claims to have",
				name, hash[:8])
		}
	}

	// The window's write should leave an orphan: bytes present, no row in the dump. Asserting it
	// makes the harmless case visible rather than incidental.
	if duringHash := captureFileRows(t, h, root)["during.txt"]; duringHash != ([32]byte{}) {
		if !blobsCopied[duringHash] {
			t.Fatalf("during.txt's bytes should be in the copy (they landed before its row, and " +
				"the copy happens after the dump); if they are not, the invariant this test rests " +
				"on has changed")
		}
		if rowsInDump["during.txt"] != ([32]byte{}) {
			t.Fatal("during.txt must not be in the dump: it was written after the dump was taken")
		}
	}
}

// TestBlobsBeforeDumpCanLoseAReference demonstrates the unsafe order.
func TestBlobsBeforeDumpCanLoseAReference(t *testing.T) {
	h := newHarness(t)
	root := h.rootNode(t)

	h.uploadFile(t, "stable.txt", "exists before the backup")

	// Artefact 1: the blob copy.
	blobsCopied := captureBlobHashes(t, h.blobRootOf(t))

	// A write inside the window.
	h.uploadFile(t, "late.txt", "row and blob both inside the window")

	// Artefact 2: the metadata dump, taken after.
	rowsInDump := captureFileRows(t, h, root)

	// In this order the copy ran before late.txt was written, so remove its bytes from the
	// captured set to represent that moment. This is the state the order produces.
	hash := rowsInDump["late.txt"]
	if hash == ([32]byte{}) {
		t.Fatal("setup: late.txt has no row, so this test cannot demonstrate anything")
	}
	delete(blobsCopied, hash)

	lost := 0
	for name, hv := range rowsInDump {
		if !blobsCopied[hv] {
			lost++
			if name == "late.txt" {
				t.Logf("lost reference: %q needs %x, which the copy does not have", name, hv[:8])
			}
		}
	}
	if lost == 0 {
		t.Fatal("the constructed unsafe state produced no lost reference, so this test no longer " +
			"demonstrates why the ordering matters")
	}
}

// TestBlobIsWrittenBeforeItsRow is the invariant the ordering argument rests on.
//
// If this ever inverts — a row inserted before its bytes are stored — the safe order flips too and
// the documentation becomes actively wrong. Pinning it means that refactor cannot happen quietly.
func TestBlobIsWrittenBeforeItsRow(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "internal", "server", "files.go"))
	if err != nil {
		t.Fatalf("read files.go: %v", err)
	}
	text := string(src)

	const simpleHandler = "func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {"
	start := strings.Index(text, simpleHandler)
	if start < 0 {
		t.Fatal("could not find handleUpload; re-derive the backup ordering argument")
	}
	// Slice to the next top-level func. Searching for "func (s *Server) " from index 1 avoids
	// matching the header of the handler itself, which would produce an empty body.
	rest := text[start+1:]
	end := strings.Index(rest, "\nfunc (s *Server) ")
	if end < 0 {
		t.Fatal("could not find the end of handleUpload; re-derive the backup ordering argument")
	}
	body := rest[:end]

	putRel := strings.Index(body, "s.deps.Content.Put(")
	publishRel := strings.Index(body, "s.publishFile(")
	if putRel < 0 || publishRel < 0 {
		t.Fatalf("handleUpload no longer both stores content (idx %d) and publishes a row "+
			"(idx %d); re-derive the backup ordering argument", putRel, publishRel)
	}
	if publishRel < putRel {
		t.Fatalf("handleUpload now publishes the metadata row BEFORE storing the content. The " +
			"safe backup order in deploy/backup/restore-drill.md is derived from the opposite " +
			"invariant: with rows first, dumping before copying blobs would produce restores that " +
			"reference bytes the copy does not have. Re-derive the order and update the document " +
			"and these tests together.")
	}
}

// TestBackupDocsStateTheRightOrder checks the documented procedure.
//
// The wording is asserted rather than reviewed, because this is exactly the sentence that reads as
// correct while being backwards — the first draft of this file had it backwards.
func TestBackupDocsStateTheRightOrder(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "deploy", "backup", "restore-drill.md"))
	if err != nil {
		t.Fatalf("read restore-drill.md: %v", err)
	}
	if !strings.Contains(string(doc), "先 pg_dump，再备份 Blob") {
		t.Fatalf("restore-drill.md must state that the database dump is taken BEFORE the blob " +
			"copy. The order is not a style choice: content is stored before its row, so dumping " +
			"first means a write in the window leaves an orphan blob (harmless), while copying " +
			"blobs first leaves a row whose bytes the copy lacks (a broken restore).")
	}
	// The backwards order may appear in the comparison table that explains why it is wrong, so the
	// check is scoped to prose that reads as an instruction: a line that does not start with a
	// table pipe. Matching the whole document would forbid the document from explaining itself.
	for _, line := range strings.Split(string(doc), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		if strings.Contains(line, "先备份 Blob，再 pg_dump") {
			t.Fatalf("restore-drill.md states the backwards order as an instruction:\n%s", line)
		}
	}
}
