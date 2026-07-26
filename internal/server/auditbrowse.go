package server

// Audit browsing.
//
// The export endpoint produces a file for an auditor. This one feeds a screen, and the
// differences matter:
//
//	export   complete, ascending, up to 100k rows, hashes included, downloads a file
//	browse   paginated, descending, newest first, with filter facets, feeds a view
//
// Both share the same scoping rule, which is the part that is easy to get wrong: a user
// sees what they did and what happened to their own data. Filtering on actor id alone hides
// the anonymous download of your own share — the single most interesting row.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// auditPageSize bounds one page of the audit view.
//
// A hundred rows is about two screens of timeline. Larger pages make the "is this all?"
// question worse without saving a request a user would notice.
const auditPageSize = 100

// auditFacetProbe is how far back the action list looks.
const auditFacetProbe = 1000

// eventJSON is one row as the view consumes it.
type eventJSON struct {
	ID         int64          `json:"id"`
	Time       string         `json:"ts"`
	ActorKind  string         `json:"actor_kind"`
	ActorID    string         `json:"actor_user_id,omitempty"`
	ActorName  string         `json:"actor_name,omitempty"`
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
}

// handleAuditList returns one page of audit events as JSON.
func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	reader, ok := s.deps.Meta.(store.AuditReader)
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported",
			"this metadata backend cannot read the audit log")
		return
	}

	filter, err := parseAuditFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}

	// A non-admin sees their own actions and everything about their own data. Without this
	// the endpoint would hand a user the whole instance's history.
	if !actor.IsAdmin() {
		id := actor.ID()
		filter.ScopeToOwner = &id
	}

	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_filter",
				"offset must be a non-negative integer")
			return
		}
		offset = n
	}

	filter.Descending = true
	filter.Offset = offset
	// One extra row answers "is there another page" without a second query.
	filter.Limit = auditPageSize + 1

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

	hasMore := len(rows) > auditPageSize
	if hasMore {
		rows = rows[:auditPageSize]
	}

	out := make([]eventJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, toEventJSON(row))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"events":    out,
		"total":     total,
		"offset":    offset,
		"page_size": auditPageSize,
		"has_more":  hasMore,
		// Facets make the filter usable: a person picks from the actions that actually
		// occurred rather than typing a name from memory and getting an empty view.
		"actions": s.auditActionFacets(r, actor, filter),
	})
}

// toEventJSON flattens a row for the wire.
func toEventJSON(row store.AuditRow) eventJSON {
	ev := eventJSON{
		ID: row.ID, Time: row.Time.UTC().Format(time.RFC3339Nano),
		ActorKind: row.ActorKind, ActorEmail: row.ActorEmail, ActorIP: row.ActorIP,
		UserAgent: row.UserAgent, Action: row.Action, TargetKind: row.TargetKind,
		TargetID: row.TargetID, Result: row.Result, Bytes: row.Bytes, Metadata: row.Metadata,
	}
	if row.ActorUserID != nil {
		ev.ActorID = row.ActorUserID.String()
	}
	if row.ShareID != nil {
		ev.ShareID = row.ShareID.String()
	}
	return ev
}

// auditActionFacets lists the actions present in the recent window.
//
// It runs a second query because neither backend can group through the row reader. One
// aggregate over the same window is a fair price for a filter that never offers a choice
// returning nothing.
func (s *Server) auditActionFacets(r *http.Request, actor authz.Actor, base store.AuditFilter) []string {
	reader, ok := s.deps.Meta.(store.AuditReader)
	if !ok {
		return nil
	}

	probe := base
	probe.Limit = auditFacetProbe
	probe.Offset = 0
	probe.Descending = true
	// The action list must not be narrowed by the action filter itself, or selecting one
	// action would remove every other from the list and the user could not switch back.
	probe.Actions = nil

	rows, err := reader.ReadAudit(r.Context(), probe)
	if err != nil {
		return nil
	}

	seen := make(map[string]struct{}, 16)
	actions := make([]string, 0, 16)
	for _, row := range rows {
		if _, dup := seen[row.Action]; dup {
			continue
		}
		seen[row.Action] = struct{}{}
		actions = append(actions, row.Action)
	}
	return actions
}
