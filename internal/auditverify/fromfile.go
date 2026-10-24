package auditverify

// Verifying an EXPORT, not a live database.
//
// §9.5 asks that an exported CSV/JSON be checkable against the original records. Two things make that
// possible without the database: each event carries its own hash, and each carries its predecessor's.
// So a file is verifiable on its own — recompute every digest from the event contents, then check that
// the links hold — and the answer does not depend on trusting the machine that produced it.
//
// Three formats exist in this system and all three are read here, because an export that can only be
// checked when it came from the one command you happen to remember is not much of a check:
//
//   - the server's JSON export  {"exported_at":..., "events":[{id, ts, ..., prev_hash, hash}]}
//   - the prune archive         one audit.Event per line (base64 hashes, Go field names)
//   - the CSV export            a header row and hex hashes
//
// The distinction the code keeps making: a file that verifies is INTERNALLY consistent. It says nothing
// about events that were removed before the file was written — that is what the external anchor is for,
// and this package's other half does that. Saying so plainly here prevents a verified export from being
// mistaken for a complete one.

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/audit"
)

// LoadFile reads an audit export in any of the formats this system writes.
func LoadFile(path string) ([]audit.Event, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auditverify: read %s: %w", path, err)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("auditverify: %s is empty; an empty log and an empty export are "+
			"different things and this cannot tell you which you have", path)
	}

	switch {
	case trimmed[0] == '{' && bytes.Contains(trimmed, []byte(`"exported_at"`)):
		return loadServerJSON(trimmed)
	case trimmed[0] == '{':
		return loadArchiveJSONL(trimmed)
	case trimmed[0] == '[':
		return loadEventArray(trimmed)
	default:
		return loadCSV(trimmed)
	}
}

// loadServerJSON reads the shape writeAuditJSON produces.
//
// hash_version, request_id and auth_method are part of the event, and an export that carries
// them must arrive here: dropping any one rebuilds a different event than the one that was
// sealed, and an honest export verifies as "modified". Older exports without the fields are
// still read — a zero version means 1, and the empty request/auth strings are exactly what a
// version-1 preimage hashes.
func loadServerJSON(body []byte) ([]audit.Event, error) {
	var doc struct {
		Events []struct {
			ID          int64          `json:"id"`
			Time        string         `json:"ts"`
			ActorKind   string         `json:"actor_kind"`
			ActorID     string         `json:"actor_user_id"`
			ActorEmail  string         `json:"actor_email"`
			ActorIP     string         `json:"actor_ip"`
			UserAgent   string         `json:"user_agent"`
			ShareID     string         `json:"share_id"`
			Action      string         `json:"action"`
			TargetKind  string         `json:"target_kind"`
			TargetID    string         `json:"target_id"`
			Result      string         `json:"result"`
			Bytes       *int64         `json:"bytes"`
			Metadata    map[string]any `json:"metadata"`
			RequestID   string         `json:"request_id"`
			AuthMethod  string         `json:"auth_method"`
			HashVersion int            `json:"hash_version"`
			PrevHash    string         `json:"prev_hash"`
			Hash        string         `json:"hash"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("auditverify: parse JSON export: %w", err)
	}
	out := make([]audit.Event, 0, len(doc.Events))
	for _, r := range doc.Events {
		ts, err := time.Parse(time.RFC3339Nano, r.Time)
		if err != nil {
			return nil, fmt.Errorf("auditverify: event %d has an unparseable ts %q: %w", r.ID, r.Time, err)
		}
		prev, err := hex.DecodeString(r.PrevHash)
		if err != nil {
			return nil, fmt.Errorf("auditverify: event %d prev_hash: %w", r.ID, err)
		}
		hash, err := hex.DecodeString(r.Hash)
		if err != nil {
			return nil, fmt.Errorf("auditverify: event %d hash: %w", r.ID, err)
		}
		e := audit.Event{
			ID: r.ID, Time: ts,
			ActorKind: audit.ActorKind(r.ActorKind), ActorEmail: r.ActorEmail,
			ActorIP: r.ActorIP, UserAgent: r.UserAgent,
			Action: audit.Action(r.Action), TargetKind: audit.TargetKind(r.TargetKind),
			TargetID: r.TargetID, Result: audit.Result(r.Result),
			Bytes: r.Bytes, Metadata: r.Metadata, PrevHash: prev, Hash: hash,
			// Zero means 1 inside the audit package; the value is carried through as read
			// so a chain that spans the version change verifies event by event.
			HashVersion: r.HashVersion,
			RequestID:   r.RequestID,
			AuthMethod:  r.AuthMethod,
		}
		if r.ShareID != "" {
			sh, serr := uuid.Parse(r.ShareID)
			if serr != nil {
				return nil, fmt.Errorf("auditverify: event %d share id: %w", r.ID, serr)
			}
			e.ShareID = &sh
		}
		if r.ActorID != "" {
			id, perr := uuid.Parse(r.ActorID)
			if perr != nil {
				return nil, fmt.Errorf("auditverify: event %d actor id: %w", r.ID, perr)
			}
			e.ActorUserID = &id
		}
		out = append(out, e)
	}
	return out, nil
}

// loadArchiveJSONL reads the one-Event-per-line format the prune writes.
func loadArchiveJSONL(body []byte) ([]audit.Event, error) {
	var out []audit.Event
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var e audit.Event
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			return nil, fmt.Errorf("auditverify: line %d: %w", line, err)
		}
		if e.ID == 0 && e.Action == "" && e.Time.IsZero() {
			return nil, fmt.Errorf("auditverify: line %d is JSON but not an audit event (no id, "+
				"action or timestamp). If this file came from an API call, it is probably an error "+
				"response saved with the wrong filename: %s", line, preview(text))
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// loadArchiveJSONL reads one event per line. A body that is a JSON object without any event fields is
// almost certainly an API error response — the live check that produced this guard saved an error
// body to a file and the verifier reported "broken at event 0", which is a false accusation rather
// than a useful answer.
func loadEventArray(body []byte) ([]audit.Event, error) {
	var out []audit.Event
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("auditverify: parse JSON array: %w", err)
	}
	return out, nil
}

// loadCSV reads the shape writeAuditCSV produces.
func loadCSV(body []byte) ([]audit.Event, error) {
	r := csv.NewReader(bytes.NewReader(body))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("auditverify: parse CSV: %w", err)
	}
	if len(rows) == 0 {
		return nil, errors.New("auditverify: CSV export has no header row")
	}
	idx := map[string]int{}
	for i, name := range rows[0] {
		idx[strings.TrimSpace(name)] = i
	}
	for _, need := range []string{"id", "ts", "action", "prev_hash", "hash"} {
		if _, ok := idx[need]; !ok {
			return nil, fmt.Errorf("auditverify: CSV export is missing the %q column", need)
		}
	}
	get := func(row []string, name string) string {
		i, ok := idx[name]
		if !ok || i >= len(row) {
			return ""
		}
		return row[i]
	}

	out := make([]audit.Event, 0, len(rows)-1)
	for n, row := range rows[1:] {
		if len(row) == 1 && strings.TrimSpace(row[0]) == "" {
			continue
		}
		var id int64
		if _, err := fmt.Sscanf(get(row, "id"), "%d", &id); err != nil {
			return nil, fmt.Errorf("auditverify: row %d has a non-numeric id %q", n+1, get(row, "id"))
		}
		ts, err := time.Parse(time.RFC3339Nano, get(row, "ts"))
		if err != nil {
			return nil, fmt.Errorf("auditverify: row %d has an unparseable ts: %w", n+1, err)
		}
		prev, err := hex.DecodeString(get(row, "prev_hash"))
		if err != nil {
			return nil, fmt.Errorf("auditverify: row %d prev_hash: %w", n+1, err)
		}
		hash, err := hex.DecodeString(get(row, "hash"))
		if err != nil {
			return nil, fmt.Errorf("auditverify: row %d hash: %w", n+1, err)
		}
		// Every field the canonical form hashes, or the row reconstructs into a different event and
		// an honest export is reported as tampered with. The CSV writer emits all of them; dropping
		// one here was a bug that only a fixture with a real user agent, address and share id showed.
		//
		// hash_version, request_id and auth_method are read BY NAME and are optional: an old
		// export without the trailing columns decodes as version 1 with empty request/auth
		// values, which is precisely the event a version-1 row was sealed from.
		e := audit.Event{
			ID: id, Time: ts,
			ActorKind:  audit.ActorKind(get(row, "actor_kind")),
			ActorEmail: get(row, "actor_email"),
			ActorIP:    get(row, "actor_ip"),
			UserAgent:  get(row, "user_agent"),
			Action:     audit.Action(get(row, "action")),
			TargetKind: audit.TargetKind(get(row, "target_kind")),
			TargetID:   get(row, "target_id"),
			Result:     audit.Result(get(row, "result")),
			RequestID:  get(row, "request_id"),
			AuthMethod: get(row, "auth_method"),
			PrevHash:   prev, Hash: hash,
		}
		if s := get(row, "hash_version"); s != "" {
			var hv int
			if _, herr := fmt.Sscanf(s, "%d", &hv); herr != nil {
				return nil, fmt.Errorf("auditverify: row %d hash_version %q: %w", n+1, s, herr)
			}
			e.HashVersion = hv
		}
		if s := get(row, "actor_user_id"); s != "" {
			uid, uerr := uuid.Parse(s)
			if uerr != nil {
				return nil, fmt.Errorf("auditverify: row %d actor id: %w", n+1, uerr)
			}
			e.ActorUserID = &uid
		}
		if s := get(row, "share_id"); s != "" {
			sh, serr := uuid.Parse(s)
			if serr != nil {
				return nil, fmt.Errorf("auditverify: row %d share id: %w", n+1, serr)
			}
			e.ShareID = &sh
		}
		if s := get(row, "bytes"); s != "" {
			var b int64
			if _, berr := fmt.Sscanf(s, "%d", &b); berr != nil {
				return nil, fmt.Errorf("auditverify: row %d bytes %q: %w", n+1, s, berr)
			}
			e.Bytes = &b
		}
		if meta := get(row, "metadata"); meta != "" {
			if err := json.Unmarshal([]byte(meta), &e.Metadata); err != nil {
				return nil, fmt.Errorf("auditverify: row %d metadata: %w", n+1, err)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// VerifyEvents checks a set of events read from a file.
//
// It recomputes every digest and checks every link, and it also reports a GAP: an event whose id is
// not one more than its predecessor's means rows are missing from the file, which the per-event hashes
// cannot reveal because each surviving row is still individually consistent.
func VerifyEvents(events []audit.Event) Report {
	var report Report
	if len(events) == 0 {
		return report
	}

	sorted := make([]audit.Event, len(events))
	copy(sorted, events)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	prev := sorted[0].PrevHash
	checked := 0
	for i := range sorted {
		e := &sorted[i]
		if !bytesEqual(e.PrevHash, prev) {
			report.Events = checked
			report.BrokenAt = e.ID
			report.Err = fmt.Errorf("prev_hash does not match the previous event (event %d)", e.ID)
			return report
		}
		if i > 0 && e.ID != sorted[i-1].ID+1 {
			// Ids come from an identity column, so a jump means events were removed between two
			// intact rows. The hashes agree because both rows are genuine; only the numbering shows
			// what is missing.
			report.Events = checked
			report.BrokenAt = e.ID
			report.Err = fmt.Errorf("event ids jump from %d to %d: rows are missing from this file",
				sorted[i-1].ID, e.ID)
			return report
		}
		stored := e.Hash
		if err := e.Seal(); err != nil {
			report.Events = checked
			report.BrokenAt = e.ID
			report.Err = fmt.Errorf("canonicalise event %d: %w", e.ID, err)
			return report
		}
		if !bytesEqual(stored, e.Hash) {
			report.Events = checked
			report.BrokenAt = e.ID
			report.Err = fmt.Errorf("hash mismatch: the record was modified (event %d)", e.ID)
			return report
		}
		prev = stored
		checked++
		if report.FirstEvent == nil {
			t := e.Time
			report.FirstEvent = &t
		}
		t := e.Time
		report.LastEvent = &t
	}
	report.Events = checked
	report.Head = prev
	return report
}

// preview shortens a line for an error message.
func preview(s string) string {
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return strings.ReplaceAll(s, "\n", " ")
}
