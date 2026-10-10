package auditverify

// Verifying an exported file (§9.5).
//
// These tests build a real chain, write it in each format the system produces, and then verify the
// FILE — because the property that matters is that an operator who receives an export can check it on
// a machine that has no database and never had one. A test that verified the in-memory events would
// prove the verifier agrees with itself.

import (
	"bytes"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/audit"
)

// buildChain makes n sealed events, each linked to the last, starting from genesis.
func buildChain(t *testing.T, n int) []audit.Event {
	t.Helper()
	events := make([]audit.Event, 0, n)
	prev := audit.GenesisHash
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		e := audit.Event{
			ID: int64(i), Time: base.Add(time.Duration(i) * time.Minute),
			ActorKind: audit.ActorUser, ActorEmail: "a@example.test",
			Action: "file.read", TargetKind: "node", TargetID: fmt.Sprintf("n%d", i),
			Result: "ok", PrevHash: prev,
		}
		if err := e.Seal(); err != nil {
			t.Fatal(err)
		}
		prev = e.Hash
		events = append(events, e)
	}
	return events
}

func writeTemp(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// serverJSON renders the shape writeAuditJSON produces.
func serverJSON(events []audit.Event) []byte {
	// Every field the canonical form hashes must be present, or the row reconstructs into a
	// DIFFERENT event and fails verification for a reason that has nothing to do with tampering.
	// The real writeAuditJSON carries all of them; this fixture has to as well.
	type row struct {
		ID         int64          `json:"id"`
		TS         string         `json:"ts"`
		ActorKind  string         `json:"actor_kind"`
		ActorEmail string         `json:"actor_email"`
		Action     string         `json:"action"`
		TargetKind string         `json:"target_kind"`
		TargetID   string         `json:"target_id"`
		Result     string         `json:"result"`
		Metadata   map[string]any `json:"metadata,omitempty"`
		PrevHash   string         `json:"prev_hash"`
		Hash       string         `json:"hash"`
	}
	doc := struct {
		ExportedAt string `json:"exported_at"`
		Total      int    `json:"total_matched"`
		Events     []row  `json:"events"`
	}{ExportedAt: "2026-03-01T00:00:00Z", Total: len(events)}
	for _, e := range events {
		doc.Events = append(doc.Events, row{
			ID: e.ID, TS: e.Time.UTC().Format(time.RFC3339Nano),
			ActorKind: string(e.ActorKind), ActorEmail: e.ActorEmail,
			Action: string(e.Action), TargetKind: string(e.TargetKind), TargetID: e.TargetID,
			Result: string(e.Result), Metadata: e.Metadata,
			PrevHash: hex.EncodeToString(e.PrevHash), Hash: hex.EncodeToString(e.Hash),
		})
	}
	b, _ := json.Marshal(doc)
	return b
}

// archiveJSONL renders the prune's one-Event-per-line format.
func archiveJSONL(events []audit.Event) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range events {
		if err := enc.Encode(events[i]); err != nil {
			panic(err)
		}
	}
	return buf.Bytes()
}

// csvExport renders the shape writeAuditCSV produces.
func csvExport(events []audit.Event) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"id", "ts", "actor_kind", "actor_user_id", "actor_email", "actor_ip",
		"user_agent", "action", "target_kind", "target_id", "share_id", "result",
		"bytes", "metadata", "prev_hash", "hash"})
	for _, e := range events {
		_ = w.Write([]string{
			fmt.Sprintf("%d", e.ID), e.Time.UTC().Format(time.RFC3339Nano),
			string(e.ActorKind), "", e.ActorEmail, "", "",
			string(e.Action), string(e.TargetKind), e.TargetID, "", string(e.Result),
			"", "{}", hex.EncodeToString(e.PrevHash), hex.EncodeToString(e.Hash),
		})
	}
	w.Flush()
	return buf.Bytes()
}

func TestEveryExportFormatVerifiesFromTheFile(t *testing.T) {
	events := buildChain(t, 5)
	formats := map[string]func([]audit.Event) []byte{
		"export.json":   serverJSON,
		"archive.jsonl": archiveJSONL,
		"export.csv":    csvExport,
	}
	exts := map[string]string{"export.json": "json", "archive.jsonl": "jsonl", "export.csv": "csv"}

	for name, render := range formats {
		t.Run(exts[name], func(t *testing.T) {
			path := writeTemp(t, name, render(events))
			loaded, err := LoadFile(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(loaded) != len(events) {
				t.Fatalf("loaded %d events, want %d", len(loaded), len(events))
			}
			report := VerifyEvents(loaded)
			if !report.OK() {
				t.Fatalf("a clean export did not verify: %v (broken at %d)", report.Err, report.BrokenAt)
			}
			if report.Events != len(events) {
				t.Fatalf("verified %d events, want %d", report.Events, len(events))
			}
		})
	}
}

func TestAModifiedExportRowIsRejected(t *testing.T) {
	events := buildChain(t, 4)
	// Change one action without recomputing the hashes: the shape of a quiet edit.
	events[2].Action = "file.delete"

	// The event's own hash no longer matches its contents in EVERY format, because all three carry
	// the stored digest. The JSON export is enough to prove the file is checked rather than trusted.
	path := writeTemp(t, "edited.json", serverJSON(events))
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	report := VerifyEvents(loaded)
	if report.OK() {
		t.Fatal("an edited event passed verification; the export would be trusted rather than checked")
	}
	if report.BrokenAt != 3 {
		t.Fatalf("reported the break at event %d, want 3", report.BrokenAt)
	}
}

func TestAJumpInEventIDsIsReportedAsMissingRows(t *testing.T) {
	events := buildChain(t, 5)
	// Remove the third event and RELINK the fourth onto the second, the way somebody who understood
	// only the hashes would. Every remaining row is internally consistent; only the numbering shows
	// what is gone.
	trimmed := append(append([]audit.Event{}, events[:2]...), events[3:]...)
	trimmed[2].PrevHash = trimmed[1].Hash
	if err := trimmed[2].Seal(); err != nil {
		t.Fatal(err)
	}
	for i := 3; i < len(trimmed); i++ {
		trimmed[i].PrevHash = trimmed[i-1].Hash
		if err := trimmed[i].Seal(); err != nil {
			t.Fatal(err)
		}
	}

	path := writeTemp(t, "gap.json", serverJSON(trimmed))
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	report := VerifyEvents(loaded)
	if report.OK() {
		t.Fatal("a file with a missing event verified; the hashes are consistent and the ids are " +
			"the only evidence, which is exactly why they are checked")
	}
	if report.BrokenAt != 4 {
		t.Fatalf("reported the break at event %d, want 4", report.BrokenAt)
	}
}

func TestAnEmptyExportIsNotTreatedAsAnIntactLog(t *testing.T) {
	path := writeTemp(t, "empty.json", []byte("   \n"))
	if _, err := LoadFile(path); err == nil {
		t.Fatal("an empty file was accepted; an empty log and an empty export are different " +
			"things and this cannot tell them apart")
	}
}

// TestFilesCarryingRealisticFieldsVerify is the case the first version of the CSV loader failed.
//
// A chain built with no IP, no user agent and no share id verifies even if the loader drops those
// columns, because the dropped values are empty on both sides. Production events have them, and the
// canonical form hashes every one — so a loader that skips a column reports a perfectly honest export
// as tampered with. This fixture carries the values a real deployment would have.
func TestFilesCarryingRealisticFieldsVerify(t *testing.T) {
	user := uuid.New()
	share := uuid.New()
	n := int64(4096)
	events := buildChain(t, 3)
	for i := range events {
		events[i].ActorUserID = &user
		events[i].ActorIP = "203.0.113.7"
		events[i].UserAgent = "Mozilla/5.0 (X11; Linux x86_64) NewCloud/1.0"
		events[i].ShareID = &share
		events[i].Bytes = &n
		events[i].Metadata = map[string]any{"path": "/reports/q1.pdf", "version": float64(2)}
		if err := events[i].Seal(); err != nil {
			t.Fatal(err)
		}
	}
	// Seal each event against its predecessor's NEW hash.
	for i := 1; i < len(events); i++ {
		events[i].PrevHash = events[i-1].Hash
		if err := events[i].Seal(); err != nil {
			t.Fatal(err)
		}
	}

	for name, render := range map[string]func([]audit.Event) []byte{
		"real.json": serverJSONReal, "real.csv": csvExportReal, "real.jsonl": archiveJSONL,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTemp(t, name, render(events))
			loaded, err := LoadFile(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			report := VerifyEvents(loaded)
			if !report.OK() {
				t.Fatalf("a realistic export did not verify: %v (broken at %d). A dropped column "+
					"reconstructs a different event; the file is honest and the reader is wrong.",
					report.Err, report.BrokenAt)
			}
		})
	}
}

// serverJSONReal mirrors writeAuditJSON including the fields a real event carries.
func serverJSONReal(events []audit.Event) []byte {
	type row struct {
		ID        int64          `json:"id"`
		TS        string         `json:"ts"`
		ActorKind string         `json:"actor_kind"`
		ActorID   string         `json:"actor_user_id,omitempty"`
		Email     string         `json:"actor_email,omitempty"`
		IP        string         `json:"actor_ip,omitempty"`
		UA        string         `json:"user_agent,omitempty"`
		Action    string         `json:"action"`
		TKind     string         `json:"target_kind"`
		TID       string         `json:"target_id,omitempty"`
		ShareID   string         `json:"share_id,omitempty"`
		Result    string         `json:"result"`
		Bytes     *int64         `json:"bytes,omitempty"`
		Metadata  map[string]any `json:"metadata,omitempty"`
		PrevHash  string         `json:"prev_hash"`
		Hash      string         `json:"hash"`
	}
	doc := struct {
		ExportedAt string `json:"exported_at"`
		Events     []row  `json:"events"`
	}{ExportedAt: "2026-03-01T00:00:00Z"}
	for _, e := range events {
		r := row{
			ID: e.ID, TS: e.Time.UTC().Format(time.RFC3339Nano),
			ActorKind: string(e.ActorKind), Email: e.ActorEmail, IP: e.ActorIP, UA: e.UserAgent,
			Action: string(e.Action), TKind: string(e.TargetKind), TID: e.TargetID,
			Result: string(e.Result), Bytes: e.Bytes, Metadata: e.Metadata,
			PrevHash: hex.EncodeToString(e.PrevHash), Hash: hex.EncodeToString(e.Hash),
		}
		if e.ActorUserID != nil {
			r.ActorID = e.ActorUserID.String()
		}
		if e.ShareID != nil {
			r.ShareID = e.ShareID.String()
		}
		doc.Events = append(doc.Events, r)
	}
	b, _ := json.Marshal(doc)
	return b
}

// csvExportReal mirrors writeAuditCSV including every hashed column.
func csvExportReal(events []audit.Event) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"id", "ts", "actor_kind", "actor_user_id", "actor_email", "actor_ip",
		"user_agent", "action", "target_kind", "target_id", "share_id", "result",
		"bytes", "metadata", "prev_hash", "hash"})
	for _, e := range events {
		var actorID, shareID, bytesField string
		if e.ActorUserID != nil {
			actorID = e.ActorUserID.String()
		}
		if e.ShareID != nil {
			shareID = e.ShareID.String()
		}
		if e.Bytes != nil {
			bytesField = fmt.Sprintf("%d", *e.Bytes)
		}
		meta, _ := json.Marshal(e.Metadata)
		_ = w.Write([]string{
			fmt.Sprintf("%d", e.ID), e.Time.UTC().Format(time.RFC3339Nano),
			string(e.ActorKind), actorID, e.ActorEmail, e.ActorIP, e.UserAgent,
			string(e.Action), string(e.TargetKind), e.TargetID, shareID, string(e.Result),
			bytesField, string(meta), hex.EncodeToString(e.PrevHash), hex.EncodeToString(e.Hash),
		})
	}
	w.Flush()
	return buf.Bytes()
}
