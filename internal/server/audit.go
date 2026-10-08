package server

// Audit export.
//
// Two formats, two audiences:
//
//   json  a machine reads it, or it is fed back into VerifyChain. Complete fidelity,
//         hashes included, so the export is independently verifiable.
//   csv   a person opens it in a spreadsheet. Flattened, hashes included for the same
//         reason: an export that cannot be checked against the live log is just a claim.
//
// The export is owner-facing and the endpoint only ever reveals what the audit log
// already records about the caller's own data — plus, for an administrator, everything.
// That distinction is enforced here, not in the query: a filter is applied to the SQL,
// and an owner's filter is forced to their own actor id.

import (
	"context"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// auditExportLimit caps how many events one export returns.
//
// A cap rather than streaming: a bounded file is one an operator can open, and an
// unbounded export from a year of activity would exhaust memory in the process serving
// requests. The response says how many matched so a partial export is never mistaken for
// a complete one.
const auditExportLimit = 100_000

// handleAuditExport produces a downloadable audit log.
func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	reader, ok := s.deps.Meta.(store.AuditReader)
	if !ok {
		// Not every backend implements reads. Reporting that plainly beats an empty file
		// that looks like "nothing happened".
		writeError(w, http.StatusNotImplemented, "unsupported",
			"this metadata backend does not support audit export")
		return
	}

	filter, err := parseAuditFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}

	// Scope before running the query.
	//
	// An ordinary user sees two kinds of events: what they did, and what was done to
	// their own data. Filtering on the actor alone would hide the second kind — the
	// anonymous download of their share, the refused attempt on their link — which is
	// precisely what a share owner opens the audit log to find. An administrator sees
	// everything, because reviewing the whole log is the job.
	if !actor.IsAdmin() {
		id := actor.ID()
		filter.ScopeToOwner = &id
	}
	filter.Limit = auditExportLimit
	filter.Descending = false

	total, err := reader.CountAudit(r.Context(), filter)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	rows, err := reader.ReadAudit(r.Context(), filter)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	// Exporting the log is itself an auditable act: a log whose readers are unrecorded
	// cannot answer "who looked at this".
	_ = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "audit.export", TargetKind: "audit", TargetID: "audit.events",
			Result: "ok",
			Metadata: map[string]any{
				"format": strings.ToLower(r.URL.Query().Get("format")),
				"rows":   len(rows),
				"total":  total,
			},
		})
	})

	format := strings.ToLower(r.URL.Query().Get("format"))
	stamp := time.Now().UTC().Format("20060102T150405Z")

	w.Header().Set("X-Audit-Matched", strconv.Itoa(total))
	w.Header().Set("X-Audit-Returned", strconv.Itoa(len(rows)))
	if total > len(rows) {
		// Make truncation impossible to miss, in the header and in the filename.
		w.Header().Set("X-Audit-Truncated", "true")
	}

	switch format {
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition",
			`attachment; filename="audit-`+stamp+`.csv"`)
		writeAuditCSV(w, rows)
	default:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition",
			`attachment; filename="audit-`+stamp+`.json"`)
		writeAuditJSON(w, rows, total)
	}
}

// parseAuditFilter reads the query string.
//
// Everything is optional; a bare request exports the caller's whole history. Invalid
// values are errors rather than being ignored: silently exporting a different range than
// the one requested is how someone draws the wrong conclusion from an audit.
func parseAuditFilter(r *http.Request) (store.AuditFilter, error) {
	q := r.URL.Query()
	var f store.AuditFilter

	if v := q.Get("since"); v != "" {
		t, err := parseAuditTime(v)
		if err != nil {
			return f, err
		}
		f.Since = &t
	}
	if v := q.Get("until"); v != "" {
		t, err := parseAuditTime(v)
		if err != nil {
			return f, err
		}
		f.Until = &t
	}
	// Accept both spellings: the export documents "actions", and a person writing a query by
	// hand reaches for the singular. Rejecting one of them produces an empty export with no
	// explanation, which is the worst possible answer.
	if v := firstNonEmpty(q.Get("actions"), q.Get("action")); v != "" {
		for _, a := range strings.Split(v, ",") {
			if a = strings.TrimSpace(a); a != "" {
				f.Actions = append(f.Actions, a)
			}
		}
	}
	if v := q.Get("result"); v != "" {
		switch v {
		case "ok", "denied":
			f.Result = v
		default:
			return f, &filterError{"result must be ok or denied"}
		}
	}
	if v := q.Get("share"); v != "" {
		id := parseUUID(v)
		if id == uuid.Nil {
			return f, &filterError{"share must be a UUID"}
		}
		f.ShareID = &id
	}
	if v := q.Get("actor"); v != "" {
		id := parseUUID(v)
		if id == uuid.Nil {
			return f, &filterError{"actor must be a UUID"}
		}
		f.ActorUserID = &id
	}
	return f, nil
}

type filterError struct{ msg string }

func (e *filterError) Error() string { return e.msg }

// parseAuditTime accepts a date or an RFC 3339 timestamp.
//
// A bare date is interpreted as midnight UTC, which is what someone typing a date into a
// query string means.
func parseAuditTime(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, &filterError{"time must be YYYY-MM-DD or RFC 3339"}
}

// writeAuditJSON emits the full structure, hashes included.
func writeAuditJSON(w http.ResponseWriter, rows []store.AuditRow, total int) {
	type jsonRow struct {
		ID         int64          `json:"id"`
		Time       string         `json:"ts"`
		ActorKind  string         `json:"actor_kind"`
		ActorID    string         `json:"actor_user_id,omitempty"`
		ActorEmail string         `json:"actor_email,omitempty"`
		ActorIP    string         `json:"actor_ip,omitempty"`
		UserAgent  string         `json:"user_agent,omitempty"`
		Action     string         `json:"action"`
		TargetKind string         `json:"target_kind"`
		TargetID   string         `json:"target_id,omitempty"`
		ShareID    string         `json:"share_id,omitempty"`
		Result     string         `json:"result"`
		Bytes      *int64         `json:"bytes,omitempty"`
		Metadata   map[string]any `json:"metadata,omitempty"`
		RequestID  string         `json:"request_id,omitempty"`
		AuthMethod string         `json:"auth_method,omitempty"`
		PrevHash   string         `json:"prev_hash"`
		Hash       string         `json:"hash"`
	}

	out := struct {
		ExportedAt string    `json:"exported_at"`
		Total      int       `json:"total_matched"`
		Returned   int       `json:"returned"`
		Truncated  bool      `json:"truncated"`
		Events     []jsonRow `json:"events"`
	}{
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Total:      total,
		Returned:   len(rows),
		Truncated:  total > len(rows),
	}

	for _, r := range rows {
		row := jsonRow{
			ID: r.ID, Time: r.Time.UTC().Format(time.RFC3339Nano),
			ActorKind: r.ActorKind, ActorEmail: r.ActorEmail, ActorIP: r.ActorIP,
			UserAgent: r.UserAgent, Action: r.Action, TargetKind: r.TargetKind,
			TargetID: r.TargetID, Result: r.Result, Bytes: r.Bytes, Metadata: r.Metadata,
			PrevHash: hex.EncodeToString(r.PrevHash), Hash: hex.EncodeToString(r.Hash),
			RequestID: r.RequestID, AuthMethod: r.AuthMethod,
		}
		if r.ActorUserID != nil {
			row.ActorID = r.ActorUserID.String()
		}
		if r.ShareID != nil {
			row.ShareID = r.ShareID.String()
		}
		out.Events = append(out.Events, row)
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

// writeAuditCSV emits a flattened table.
func writeAuditCSV(w http.ResponseWriter, rows []store.AuditRow) {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	_ = cw.Write([]string{
		"id", "ts", "actor_kind", "actor_user_id", "actor_email", "actor_ip",
		"user_agent", "action", "target_kind", "target_id", "share_id", "result",
		"bytes", "metadata", "prev_hash", "hash",
	})

	for _, r := range rows {
		var actorID, shareID string
		if r.ActorUserID != nil {
			actorID = r.ActorUserID.String()
		}
		if r.ShareID != nil {
			shareID = r.ShareID.String()
		}
		bytesField := ""
		if r.Bytes != nil {
			bytesField = strconv.FormatInt(*r.Bytes, 10)
		}
		// Metadata is serialised as JSON in one cell rather than exploded into columns:
		// the keys differ per action, and a union of all of them would be unusable.
		meta := ""
		if len(r.Metadata) > 0 {
			if b, err := json.Marshal(r.Metadata); err == nil {
				meta = string(b)
			}
		}

		_ = cw.Write([]string{
			strconv.FormatInt(r.ID, 10),
			r.Time.UTC().Format(time.RFC3339Nano),
			r.ActorKind, actorID, r.ActorEmail, r.ActorIP, r.UserAgent,
			r.Action, r.TargetKind, r.TargetID, shareID, r.Result, bytesField, meta,
			hex.EncodeToString(r.PrevHash), hex.EncodeToString(r.Hash),
		})
	}
}

// firstNonEmpty returns the first non-empty string, for query parameters that have an
// alias.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
