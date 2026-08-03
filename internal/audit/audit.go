// Package audit records security-relevant events with a tamper-evident hash
// chain.
//
// Two properties matter here:
//
//  1. Nothing happens without a record. Events are written in the same
//     transaction as the business change (see the outbox pattern in store), so
//     a rolled-back operation cannot leave an audit trail claiming it happened,
//     and a committed operation cannot escape without one.
//
//  2. Editing history is detectable. Each row commits to its predecessor's
//     hash, so altering or deleting a row breaks the chain from that point on.
//     This does *not* make the log immutable by itself — an attacker with write
//     access can rewrite the whole tail. That is why the daily anchor writes the
//     chain head somewhere the application cannot reach (see Anchor).
package audit

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Action is a stable, dotted event name. Never reuse or rename one: audit
// queries and retention rules key off these strings.
type Action string

const (
	// Authentication.
	ActionLoginSuccess      Action = "auth.login.success"
	ActionLoginFailure      Action = "auth.login.failure"
	ActionLogout            Action = "auth.logout"
	ActionSessionRevoke     Action = "auth.session.revoke"
	ActionAppPasswordCreate Action = "auth.apppassword.create"
	ActionAppPasswordRevoke Action = "auth.apppassword.revoke"
	ActionBreakglassUsed    Action = "auth.breakglass.used"
	ActionBreakglassDenied  Action = "auth.breakglass.denied"

	// Shares.
	ActionShareCreate            Action = "share.create"
	ActionShareUpdate            Action = "share.update"
	ActionShareRevoke            Action = "share.revoke"
	ActionSharePasswordFailure   Action = "share.password.verify.failure"
	ActionShareExpired           Action = "share.expired"
	ActionShareLimitReached      Action = "share.limit_reached"
	ActionShareDownload          Action = "share.download"
	ActionShareView              Action = "share.view"
	ActionShareUpload            Action = "share.upload"
	ActionShareRecipientInvited  Action = "share.recipient.invited"
	ActionShareRecipientVerified Action = "share.recipient.verified"
	ActionShareRecipientDenied   Action = "share.recipient.denied"

	// Files.
	ActionFileRead        Action = "file.read"
	ActionFileWrite       Action = "file.write"
	ActionFileDelete      Action = "file.delete"
	ActionFileRestore     Action = "file.restore"
	ActionFileMove        Action = "file.move"
	ActionFileDownloadZip Action = "file.download_zip"
	ActionFileVersionRead Action = "file.version.read"

	// Administration.
	ActionUserCreate Action = "user.create"
	// ActionUserRoleChange records an IdP-driven role change. A downgrade is the
	// security-relevant case, so it is a first-class event rather than a log line:
	// "who lost admin, when, and did their credentials actually stop working".
	ActionUserRoleChange    Action = "user.role.change"
	ActionUserDisable       Action = "user.disable"
	ActionUserQuotaChange   Action = "user.quota.change"
	ActionGroupMemberChange Action = "group.member.change"
	ActionSettingChange     Action = "admin.setting.change"
	ActionAuditExport       Action = "audit.export"
	ActionPrivilegedRead    Action = "admin.privileged_read"

	// Anomalies.
	ActionAnomaly Action = "security.anomaly"
)

// Result is the outcome of the attempted action.
type Result string

const (
	ResultOK     Result = "ok"
	ResultDenied Result = "denied"
	ResultError  Result = "error"
)

// TargetKind classifies what the action touched.
type TargetKind string

const (
	TargetNode    TargetKind = "node"
	TargetShare   TargetKind = "share"
	TargetUser    TargetKind = "user"
	TargetGroup   TargetKind = "group"
	TargetSetting TargetKind = "setting"
	TargetSession TargetKind = "session"
	TargetNone    TargetKind = "none"
)

// ActorKind distinguishes who acted. Anonymous matters: file-request uploads and
// link downloads have no user account but must still be attributable to an IP.
type ActorKind string

const (
	ActorUser      ActorKind = "user"
	ActorAnonymous ActorKind = "anonymous"
	ActorSystem    ActorKind = "system"
	ActorAdmin     ActorKind = "admin"
)

// Event is one audit record. Metadata carries the free-form details
// (filename snapshot, old/new values, failure reason) and is canonicalised
// before hashing so that key order never affects the chain.
type Event struct {
	ID          int64
	Time        time.Time
	ActorKind   ActorKind
	ActorUserID *uuid.UUID
	ActorEmail  string
	ActorIP     string
	UserAgent   string
	Action      Action
	TargetKind  TargetKind
	TargetID    string
	ShareID     *uuid.UUID
	Result      Result
	Bytes       *int64
	Metadata    map[string]any
	PrevHash    []byte
	Hash        []byte
}

// GenesisHash is the chain seed. Using a fixed non-zero value makes an empty
// chain distinguishable from a truncated one.
var GenesisHash = bytes32(0x00)

func bytes32(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

// Canonical serialises the fields that participate in the hash.
//
// The encoding is deliberately explicit rather than a struct marshalled with
// reflection, because adding a field to Event must not silently change the hash
// of historical rows. Changing this function is a schema migration, not a
// refactor.
func (e *Event) canonical() ([]byte, error) {
	meta, err := canonicalJSON(e.Metadata)
	if err != nil {
		return nil, err
	}

	var buf []byte
	writeStr := func(s string) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		buf = append(buf, n[:]...)
		buf = append(buf, s...)
	}
	writeBytes := func(b []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(b)))
		buf = append(buf, n[:]...)
		buf = append(buf, b...)
	}

	// Second precision: two events in the same second must still chain
	// deterministically, and sub-second precision differs between DB and Go.
	writeStr(e.Time.UTC().Format(time.RFC3339))
	writeStr(string(e.ActorKind))
	if e.ActorUserID != nil {
		writeStr(e.ActorUserID.String())
	} else {
		writeStr("")
	}
	writeStr(e.ActorEmail)
	writeStr(normaliseIP(e.ActorIP))
	writeStr(e.UserAgent)
	writeStr(string(e.Action))
	writeStr(string(e.TargetKind))
	writeStr(e.TargetID)
	if e.ShareID != nil {
		writeStr(e.ShareID.String())
	} else {
		writeStr("")
	}
	writeStr(string(e.Result))
	if e.Bytes != nil {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(*e.Bytes))
		writeBytes(b[:])
	} else {
		writeStr("")
	}
	writeBytes(meta)

	return buf, nil
}

// Seal computes e.Hash from e.PrevHash and the canonical form of the event.
func (e *Event) Seal() error {
	body, err := e.canonical()
	if err != nil {
		return fmt.Errorf("audit: canonicalise: %w", err)
	}
	h := sha256.New()
	h.Write(e.PrevHash)
	h.Write(body)
	e.Hash = h.Sum(nil)
	return nil
}

// normaliseIP makes the address hash-identical on the way in and on the way out.
//
// This is not cosmetic. The column is PostgreSQL `inet`, which renders a plain
// address with its prefix length: `127.0.0.1` is stored as `127.0.0.1/32` and read
// back that way. The writer hashed the string it was handed ("127.0.0.1") while the
// verifier hashed what the database returned ("127.0.0.1/32"), so *every* event
// carrying an address failed verification — the chain looked tampered-with while it
// was in fact intact.
//
// The fix normalises the value rather than changing the column: an audit row must
// keep hashing the same way forever, and rewriting existing hashes is exactly the
// operation the chain exists to make impossible.
func normaliseIP(ip string) string {
	// A netip.Addr always formats without a prefix length, and parsing accepts the
	// "/32" form PostgreSQL adds, so both sides converge on one representation.
	if addr, err := netip.ParsePrefix(ip); err == nil {
		return addr.Addr().String()
	}
	if addr, err := netip.ParseAddr(ip); err == nil {
		return addr.String()
	}
	return ip
}

// canonicalJSON produces a deterministic byte encoding of metadata:
// sorted keys, no insignificant whitespace.
func canonicalJSON(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// encoding/json sorts map keys already, so marshalling is deterministic.
	// The explicit sort above documents the dependency.
	return json.Marshal(m)
}

// VerifyChain re-computes the chain and returns the index of the first event
// that fails. n == -1 means the chain is intact.
func VerifyChain(events []Event) (badIndex int, err error) {
	prev := GenesisHash
	for i := range events {
		e := &events[i]
		if !equalBytes(e.PrevHash, prev) {
			return i, fmt.Errorf("audit: event %d: prev_hash does not match chain", i)
		}
		orig := e.Hash
		if err := e.Seal(); err != nil {
			return i, err
		}
		if !equalBytes(orig, e.Hash) {
			return i, fmt.Errorf("audit: event %d: hash mismatch (record was modified)", i)
		}
		prev = orig
	}
	return -1, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Head returns the last hash in the chain, or GenesisHash when empty. This value
// is what Anchor publishes externally.
func Head(events []Event) []byte {
	if len(events) == 0 {
		return GenesisHash
	}
	return events[len(events)-1].Hash
}

// Anchor receives the daily chain head. Implementations must store it where the
// application process has no write access (object storage with append-only
// policy, WORM volume, or an email to the security mailbox); otherwise anchor
// and log can be rewritten together.
type Anchor interface {
	Publish(day time.Time, head []byte) error
}
