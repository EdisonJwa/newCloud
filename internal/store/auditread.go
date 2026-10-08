package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AuditFilter selects audit events for reading.
//
// Every field is optional and the zero value means "no restriction", except TimeRange
// where an open end is expressed by leaving the bound nil. The filter is a value rather
// than a variadic option list so a caller building it from HTTP query parameters can be
// validated in one place.
type AuditFilter struct {
	// Since and Until bound the window, inclusive of Since and exclusive of Until.
	Since *time.Time
	Until *time.Time
	// Actions restricts to specific action names.
	Actions []string
	// ActorUserID restricts to one account.
	ActorUserID *uuid.UUID
	// ShareID restricts to events about one share. This is the "what happened to the
	// link I sent" query, which is the one an owner actually asks.
	ShareID *uuid.UUID
	// Result restricts to ok or denied.
	Result string
	// ScopeToOwner restricts to events about the owner's own data, in addition to the
	// events the owner caused.
	//
	// It is not the same as ActorUserID. ActorUserID answers "what did this person do",
	// which for a share owner omits the most interesting rows: the anonymous visitor who
	// downloaded the link, and the refused attempt from an address that was never
	// invited. Both are events about the owner's data with no actor account at all, so a
	// pure actor filter hides exactly what the owner came to look at.
	ScopeToOwner *uuid.UUID
	// Limit caps the number of rows. Zero means "the caller's default", which the
	// exporter sets; an unbounded export is how a query takes down a database.
	Limit int
	// Offset skips matching rows before applying Limit. It exists for paging the live view,
	// where the reader walks backwards through history.
	//
	// Paging by offset is only correct because the sort is by id and the table is
	// append-only: a new event shifts the window by one, which is the expected behaviour for
	// someone reading recent history. It would not be correct for a stable snapshot.
	Offset int
	// Descending orders newest first. Exports use ascending so the file reads
	// chronologically; the live view uses descending.
	Descending bool
}

// AuditRow is one event as read back.
//
// It mirrors audit.Event without importing that package: the store must not depend on
// the audit writer, and the two shapes are kept deliberately parallel.
type AuditRow struct {
	ID          int64
	Time        time.Time
	ActorKind   string
	ActorUserID *uuid.UUID
	ActorEmail  string
	ActorIP     string
	UserAgent   string
	Action      string
	TargetKind  string
	TargetID    string
	ShareID     *uuid.UUID
	Result      string
	Bytes       *int64
	Metadata    map[string]any
	PrevHash    []byte
	Hash        []byte
	// RequestID correlates the event with the request that produced it, and is empty for events
	// written outside one.
	RequestID string
	// AuthMethod is how the actor authenticated, empty when no authentication was involved.
	AuthMethod string
}

// ReadAudit is the store-level read path for the audit log.
//
// It lives on Store rather than Tx because an export can be large: holding a write
// transaction open while streaming a year of events would block every mutation.
type AuditReader interface {
	ReadAudit(ctx context.Context, f AuditFilter) ([]AuditRow, error)
	// CountAudit reports how many rows match, so an exporter can warn before producing
	// a multi-gigabyte file.
	CountAudit(ctx context.Context, f AuditFilter) (int, error)
}
