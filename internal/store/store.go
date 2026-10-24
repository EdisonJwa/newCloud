// Package store is the metadata layer: the node tree, quotas, versions and the
// PostgreSQL-backed resolver that the blob storage layout needs.
//
// Layering rule enforced here: this package never decides authorization. It
// accepts plain IDs (and, where a caller has already been authorized, a
// zero-sized proof), never an Actor. That way a handler cannot "accidentally"
// query data it was not allowed to see by passing a richer object.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/consistency"
)

// Kind distinguishes files from directories.
type Kind string

const (
	KindFile Kind = "file"
	KindDir  Kind = "dir"
)

// Node is one entry in the tree.
type Node struct {
	ID         uuid.UUID
	ParentID   *uuid.UUID
	OwnerID    uuid.UUID
	Name       string
	Kind       Kind
	Size       int64
	Mime       string
	ETag       string
	BlobID     *uuid.UUID
	TrashedAt  *time.Time
	CreatedAt  time.Time
	ModifiedAt time.Time
	// RetainUntil is when this node is purged, set at delivery time from the share that
	// accepted it (SHARE-05 保留期限). Nil means no retention was in force.
	RetainUntil *time.Time
	// OwnerGroupID names the group that owns this node, when it is a group space rather than a
	// personal one. The column was stored and scanned away for a while: writes set it and reads
	// dropped it, so nothing could ever report a space.
	OwnerGroupID *uuid.UUID
}

// IsDir reports whether the node is a directory.
func (n Node) IsDir() bool { return n.Kind == KindDir }

// Path reconstructs a human-readable path for logging and audit metadata. It is
// NOT an identifier and must never be used for authorization: paths change and
// two nodes can legitimately have had the same path at different times.
func Path(ancestors []Node, leaf Node) string {
	out := "/"
	for _, a := range ancestors {
		out += a.Name + "/"
	}
	return out + leaf.Name
}

// Errors returned by every Store implementation. Handlers map these to HTTP
// status codes in exactly one place.
var (
	// ErrNotFound: no such node.
	ErrNotFound = errors.New("store: not found")
	// ErrInvalidCursor reports a page cursor this store did not issue, or one it can no longer
	// parse. Distinguishable from a malformed request so the HTTP layer can answer 400 rather than
	// silently starting from the beginning.
	ErrInvalidCursor = errors.New("store: invalid cursor")
	// ErrConflict: a sibling with that name already exists (or another
	// uniqueness rule was violated).
	ErrConflict = errors.New("store: conflict")
	// ErrNotEmpty: attempted an operation that requires an empty directory.
	ErrNotEmpty = errors.New("store: directory not empty")
	// ErrQuota: the write would exceed quota.
	ErrQuota = errors.New("store: quota exceeded")
	// ErrAuditUnavailable: the operation cannot be recorded, so it is refused (T-16).
	//
	// Distinct from a generic failure because the policy is fail-closed: the write did not
	// happen and retrying will not help until audit storage recovers. A caller that gets this
	// must not treat it as a transient error on the data path — the data is fine; the logger is
	// not.
	ErrAuditUnavailable = errors.New("store: audit storage unavailable")
	// ErrTooLarge: the file exceeds the configured per-file limit.
	ErrTooLarge = errors.New("store: file too large")
	// ErrInvalidName: the name or path is not representable.
	ErrInvalidName = errors.New("store: invalid name")
	// ErrCycle: attempted to move a directory into its own subtree.
	ErrCycle = errors.New("store: would create a cycle")
	// ErrNestedTx: a transaction callback tried to start another transaction.
	// Neither backend supports nesting, so this is a programming error rather
	// than a runtime condition.
	ErrNestedTx = errors.New("store: nested transaction")
	// ErrPrecondition: the target no longer matches what the caller expected.
	// It is what a lost optimistic-concurrency race returns, and it maps to 412
	// on every entry point. Returning a plain conflict instead would be wrong:
	// a conflict means the name is taken, this means the bytes changed under you.
	ErrPrecondition = errors.New("store: precondition failed")
	// ErrUnavailable: the store cannot be reached. It is distinct from a query
	// returning nothing, and health checks are the main consumer.
	ErrUnavailable = errors.New("store: unavailable")
)

// Precondition is the caller's expectation about the target's current state.
//
// It exists so that conditional writes are decided in the statement that changes
// the row, not by a read in the handler followed by a write somewhere else. That
// read-then-write shape is the classic lost-update bug: two writers both observe
// the same ETag, both pass their own check, and the second silently discards the
// first one's work.
type Precondition struct {
	// IfMatch requires the current ETag to equal this value. "*" means "the
	// target must already exist". Empty means no requirement.
	IfMatch string
	// IfNoneMatch requires the current ETag to differ. "*" means "the target
	// must not exist", which is how a create-only write is expressed. Empty
	// means no requirement.
	IfNoneMatch string
}

// Matches evaluates the precondition against the target's current ETag.
// exists reports whether the target is present at all.
func (p *Precondition) Matches(etag string, exists bool) bool {
	if p == nil {
		return true
	}
	if p.IfNoneMatch != "" {
		if !exists {
			// Nothing to match against: the condition "must not be this ETag"
			// holds trivially.
		} else if p.IfNoneMatch == "*" || p.IfNoneMatch == etag {
			return false
		}
	}
	if p.IfMatch != "" {
		if !exists {
			return false
		}
		if p.IfMatch != "*" && p.IfMatch != etag {
			return false
		}
	}
	return true
}

// Tx is a unit of work. Every mutating operation runs inside one, together with
// its audit event and any jobs it enqueues. That is what makes "no operation
// without an audit record" true rather than aspirational.
type Tx interface {
	// Node fetches a live (non-trashed) node.
	Node(ctx context.Context, id uuid.UUID) (Node, error)
	// NodeIncludingTrashed fetches a node in any state.
	//
	// It is separate from Node so the common path cannot accidentally serve a
	// deleted node, and it is on the interface because the restore flow has to
	// read the row before it can decide whether the caller owns it.
	NodeIncludingTrashed(ctx context.Context, id uuid.UUID) (Node, error)
	// Children returns direct children of a directory, ordered by name.
	Children(ctx context.Context, parentID uuid.UUID, limit int) ([]Node, error)
	// AllRoots returns every user's namespace root, for whole-deployment operations like
	// the restore drill.
	AllRoots(ctx context.Context) ([]Node, error)
	// Descendants walks a whole subtree, which a recursive operation needs in order to
	// check inner protections rather than only the root (§4.2).
	Descendants(ctx context.Context, rootID uuid.UUID, limit int) ([]Node, bool, error)
	// ChildrenAfter returns direct children whose name sorts strictly after
	// `after`, ordered by name.
	//
	// This is keyset pagination, and the choice is not cosmetic: an OFFSET over a
	// name sort skips or repeats rows whenever a sibling is added or removed
	// between pages, which is exactly what T-19 forbids. "The next page starts
	// after this name" stays correct under concurrent inserts because the sort key
	// is the cursor.
	ChildrenAfter(ctx context.Context, parentID uuid.UUID, after string, limit int) ([]Node, error)
	// ChildByName resolves a single child; used by path resolution.
	ChildByName(ctx context.Context, parentID uuid.UUID, name string) (Node, error)

	// CreateDir creates a directory.
	CreateDir(ctx context.Context, n NewNode) (Node, error)
	// CreateFile records a file whose bytes are already stored as blobID. The
	// caller must have called blob.Put first; if this transaction rolls back the
	// blob is orphaned and the refcount sweep collects it.
	CreateFile(ctx context.Context, n NewFile) (Node, error)
	// WriteFile creates a file, or replaces the content of the live sibling with
	// the same name, honouring the precondition in the same statement that
	// changes the row.
	//
	// This is the one write path every entry point uses: the browser upload, the
	// resumable upload's commit, and WebDAV PUT. Having three separate
	// implementations is how one of them ends up without a precondition and
	// silently overwrites another client's work.
	WriteFile(ctx context.Context, w WriteFile) (node Node, created bool, err error)
	// UpdateFile swaps a file's content, optionally keeping the old content as
	// a version.
	UpdateFile(ctx context.Context, u UpdateFile) (Node, error)

	// Rename changes a node's name.
	Rename(ctx context.Context, id uuid.UUID, name string) (Node, error)
	// Move reparents a node.
	Move(ctx context.Context, id, newParent uuid.UUID) (Node, error)
	// Trash soft-deletes a node and, for a directory, its whole subtree.
	Trash(ctx context.Context, id uuid.UUID) error
	// Restore un-deletes a node back to its original parent.
	Restore(ctx context.Context, id uuid.UUID) (Node, error)
	// Trashed lists an owner's trashed nodes, newest first.
	//
	// Only the roots of trashed subtrees are returned: listing every descendant
	// would show a directory and its children as independent items, and restoring
	// one child on its own is not something the UI can sensibly offer.
	Trashed(ctx context.Context, ownerID uuid.UUID, limit int) ([]Node, error)
	// TrashedAfter pages the trash, newest first, strictly after cursor. An empty cursor starts at
	// the newest entry; the cursor comes from FormatTrashCursor, which the store owns because the
	// ordering it has to match lives here.
	TrashedAfter(ctx context.Context, ownerID uuid.UUID, cursor string, limit int) ([]Node, error)
	// SpacesFor lists the group-owned roots this account can see.
	//
	// A space is a node whose owner_group_id names a group the caller belongs to. Membership is the
	// whole test: the space exists for the group, not for whoever happened to create it, and a
	// member who joins later sees it without anybody granting anything.
	SpacesFor(ctx context.Context, userID uuid.UUID) ([]Node, error)
	// The operation journal (ADR-0013 4.1). The table is migration 0016; these are the transitions
	// that make it usable, so a crash between moving bytes and committing metadata can be repaired
	// rather than guessed at.
	StartOp(ctx context.Context, op Op) (Op, error)
	AdvanceOp(ctx context.Context, id uuid.UUID, from, to string) error
	OpByID(ctx context.Context, id uuid.UUID) (Op, error)
	OpenOps(ctx context.Context, limit int) ([]Op, error)
	// OpsForNode lists the operations recorded against a node, newest first. Recovery asks this to
	// find out what was being done to a node whose row is present but whose content is not where the
	// metadata expects.
	OpsForNode(ctx context.Context, nodeID uuid.UUID) ([]Op, error)

	// Versions lists kept versions of a file, newest first.
	Versions(ctx context.Context, nodeID uuid.UUID, limit int) ([]Version, error)
	// RestoreVersion makes an old version current, keeping the current one.
	RestoreVersion(ctx context.Context, nodeID uuid.UUID, versionNo int, by *uuid.UUID) (Node, error)

	// Audit appends an event to the audit log in the same transaction.
	Audit(ctx context.Context, e AuditEntry) error

	// CompleteOperation moves one PUBLISHING operation to COMMITTED in the same
	// transaction as the visibility change. It refuses an operation whose owner
	// or kind differs from the publish request, and it refuses an empty id:
	// publication without a journal row is not recoverable.
	CompleteOperation(ctx context.Context, opID, ownerID uuid.UUID, kind string) error

	// EnsureRoot provisions the owner's root directory inside the transaction, so
	// an account cannot exist without a place to put files.
	EnsureRoot(ctx context.Context, ownerID uuid.UUID) (Node, error)

	// Session operations live on the transaction so that creating a session and
	// recording the login event commit or roll back together. A session that
	// exists without an audit record would be an unaccounted-for credential.
	CreateSession(ctx context.Context, s Session) error
	SessionLive(ctx context.Context, jti string) (bool, error)
	RevokeSession(ctx context.Context, jti string) error
	RevokeUserSessions(ctx context.Context, userID string) error
	// PruneSessions deletes dead session rows past the retention horizon, so the table
	// stays proportional to active users instead of to every login ever made. The
	// retention rule lives with the implementations (see identity_prune.go).
	PruneSessions(ctx context.Context, olderThan time.Duration) (int64, error)

	// UserByID and UserByIdPSubject are the identity reads used during login.
	UserByID(ctx context.Context, id uuid.UUID) (User, error)
	UserByIdPSubject(ctx context.Context, issuer, subject string) (User, error)
	// UpsertIdPUser provisions or refreshes an account from IdP claims.
	UpsertIdPUser(ctx context.Context, u UpsertUser) (User, error)
	// CreateUser provisions a NEW account from an administrator's request (ID-09). It never
	// modifies an existing row: duplicates are ErrConflict, not updates.
	CreateUser(ctx context.Context, u UpsertUser) (User, error)
	// ApplyIdPRole records the role the IdP now claims and revokes live
	// credentials on a downgrade.
	ApplyIdPRole(ctx context.Context, userID uuid.UUID, role string) (RoleChange, error)
	// MarkIdPConfirmed records that the IdP has just asserted this identity's claims, which is what
	// makes the revalidation window measurable (ID-06, WC-GAP-001).
	MarkIdPConfirmed(ctx context.Context, userID uuid.UUID) error

	// App password operations, on the transaction so that issuing a credential
	// and recording the audit event commit together.
	CreateAppPassword(ctx context.Context, p AppPassword) error
	AppPasswordByHash(ctx context.Context, hash []byte) (AppPassword, error)
	TouchAppPassword(ctx context.Context, id uuid.UUID, ip string) error
	RevokeAppPassword(ctx context.Context, id uuid.UUID) error
	// RevokeUserAppPasswords kills every live credential of an account, which is
	// what "the account was disabled" has to mean for WebDAV clients.
	RevokeUserAppPasswords(ctx context.Context, userID uuid.UUID) (int, error)
	ListAppPasswords(ctx context.Context, userID uuid.UUID) ([]AppPassword, error)

	// Nextcloud login-flow-v2 state (ADR-0015). The poll deletes the row and the caller
	// mints the credential in the SAME request, so a flow cannot be consumed twice; the
	// approval is conditional for the same reason.
	CreateLoginFlow(ctx context.Context, f LoginFlow, pollHash, loginHash [32]byte) error
	LoginFlowByLoginHash(ctx context.Context, loginHash [32]byte) (LoginFlow, error)
	ApproveLoginFlow(ctx context.Context, loginHash [32]byte, userID uuid.UUID, at time.Time) (LoginFlow, error)
	ConsumeLoginFlow(ctx context.Context, pollHash [32]byte, at time.Time) (LoginFlow, error)
	DeleteExpiredLoginFlows(ctx context.Context, now time.Time) (int, error)

	// WebDAV lock operations. Locks are durable because an in-memory lock system
	// breaks under restart or a second process, silently corrupting co-edited
	// documents.
	CreateLock(ctx context.Context, l Lock) error
	ActiveLock(ctx context.Context, nodeID uuid.UUID) (Lock, error)
	RefreshLock(ctx context.Context, nodeID uuid.UUID, token string, userID uuid.UUID, expires time.Time) (Lock, error)
	DeleteLock(ctx context.Context, nodeID uuid.UUID, token string, userID uuid.UUID) (bool, error)
	ExpireLocks(ctx context.Context) (int, error)
	LocksForUser(ctx context.Context, userID uuid.UUID) ([]Lock, error)
	// LocksInSubtree lists every live lock on the node and its descendants, which is
	// what DELETE and MOVE of a collection must consult before destroying or moving
	// content somebody else holds a lock on.
	LocksInSubtree(ctx context.Context, rootID uuid.UUID) ([]Lock, error)

	// TrashCappedExpect moves a node to the trash only if it still matches expect.
	//
	// The precondition is what makes DELETE safe against a concurrent writer. A handler
	// resolves the node, then trashes it in a later transaction; a PUT that updates that
	// row in between is trashed with it, and the writer's success disappears from the
	// namespace with no error addressed to it. Checking in the handler has the same
	// read-then-write shape — the decision has to be in the statement that changes the
	// row, exactly like UpdateFile.Expect.
	//
	// A nil expect is unconditional, which is what TrashCapped means.
	TrashCappedExpect(ctx context.Context, id uuid.UUID, maxRetainedBytes int64, expect *Precondition) error

	// WebDAV dead properties. Windows Explorer and macOS Finder store per-file
	// metadata here and misbehave when it does not survive a reconnect.
	SetDeadProp(ctx context.Context, nodeID uuid.UUID, space, name, value string) error
	DeadProps(ctx context.Context, nodeID uuid.UUID) ([]DeadProp, error)
	DeadPropsFor(ctx context.Context, nodeIDs []uuid.UUID) (map[uuid.UUID][]DeadProp, error)
	DeleteDeadProp(ctx context.Context, nodeID uuid.UUID, space, name string) error

	// Share operations.
	CreateShare(ctx context.Context, s Share) (Share, error)
	ShareByTokenHash(ctx context.Context, hash []byte) (Share, error)
	ShareByID(ctx context.Context, id uuid.UUID) (Share, error)
	UpdateShare(ctx context.Context, s Share) (Share, error)
	RevokeShare(ctx context.Context, id uuid.UUID) error
	IncrementDownload(ctx context.Context, id uuid.UUID) error
	IncrementUpload(ctx context.Context, id uuid.UUID, size int64) error
	SharesForNode(ctx context.Context, nodeID uuid.UUID) ([]Share, error)
	SharesByOwner(ctx context.Context, ownerID uuid.UUID) ([]Share, error)
	AddRecipient(ctx context.Context, r ShareRecipient) error
	RecipientsFor(ctx context.Context, shareID uuid.UUID) ([]ShareRecipient, error)
	RecipientFor(ctx context.Context, shareID uuid.UUID, email string) (ShareRecipient, error)
	MarkRecipientVerified(ctx context.Context, shareID uuid.UUID, email string) error
	MarkRecipientAccess(ctx context.Context, shareID uuid.UUID, email string) error

	// Blob lifecycle, used by the maintenance worker.
	//
	// The grace period is part of the query rather than a caller-side filter: a
	// blob must not be collected while a user can still restore the file that
	// referenced it.
	CollectableBlobs(ctx context.Context, grace time.Duration) ([]BlobRef, error)
	DeleteBlob(ctx context.Context, id uuid.UUID) error
	// BlobHash returns the content address recorded for a blob, for verification that does
	// not trust either copy on disk.
	BlobHash(ctx context.Context, id uuid.UUID) ([]byte, error)
	// TrimRetainedVersions drops the oldest version rows until every account's retained history fits
	// capBytes (§4.3). A cap of zero or less is a no-op.
	//
	// Oldest-first rather than newest-first because history is a stack: the version a user is most
	// likely to want back is the one before the current content, which is the newest entry.
	//
	// This is a sweep rather than a check on the write path. Refusing a save because the account's
	// HISTORY is full would lose the content the user is actually trying to write; trimming is the
	// policy every product with versioning converges on, and it is a retention limit rather than a
	// billing one, so a delay until the next sweep is acceptable.
	TrimRetainedVersions(ctx context.Context, capBytes int64) (int, error)

	// TrashCapped moves a node to the trash, refusing when the account's RETAINED usage would exceed
	// maxRetainedBytes (§4.3: trash is retained usage with its own cap, separate from the logical
	// quota).
	//
	// Refusing rather than auto-purging the oldest entry is deliberate: deleting a file the user did
	// not choose is data loss, and a full bin is a condition they can fix in one action. A cap of
	// zero or less means unlimited, which is the default for deployments that have not opted in.
	//
	// The check and the move are in one transaction, so two concurrent deletes cannot both pass a
	// check that only one of them fits.
	TrashCapped(ctx context.Context, id uuid.UUID, maxRetainedBytes int64) error

	// ReadSettings returns every runtime setting that has been written.
	//
	// The settings table has existed since the first migration and had no Go code touching it: the
	// schema promised runtime configuration and the application never delivered it, so every limit
	// change was a restart.
	ReadSettings(ctx context.Context) (map[string]json.RawMessage, error)

	// WriteSetting stores one runtime setting, replacing any previous value.
	//
	// The value is validated by the caller BEFORE it reaches here: this method cannot know what a
	// particular key means, and storing an unvalidated value would push the failure to the request
	// that later reads it.
	WriteSetting(ctx context.Context, key string, value json.RawMessage, by uuid.UUID) error

	// DeleteSetting removes an override, returning the key to the configured default.
	DeleteSetting(ctx context.Context, key string) error

	// ListOwnerNodes returns every live node an account owns, for a portable export (§9.5).
	//
	// Trashed nodes are excluded: the export is what the user still has, and including the bin would
	// resurrect content they deleted. The ordering is stable (parent, then name) so two exports of an
	// unchanged tree are byte-identical apart from the manifest timestamp, which is what makes them
	// comparable.
	ListOwnerNodes(ctx context.Context, ownerID uuid.UUID) ([]Node, error)

	// SetUserQuota changes an account's byte limit. Zero or negative means unlimited.
	//
	// There was NO way to change an existing account's quota: limits.default_file_quota_bytes is
	// applied once, when the account is provisioned, and nothing afterwards could raise or lower it.
	// An operator whose user hit the limit had two options — edit the database, or tell the user to
	// delete files — and the second is not a policy.
	//
	// Lowering the limit below current usage is allowed and must not be "fixed" by deleting
	// anything: the account is over its limit and the next write is refused, which is the point.
	SetUserQuota(ctx context.Context, userID uuid.UUID, bytes int64) error

	// SampleBlobHashes returns up to limit content addresses ordered after `after`, for an integrity
	// scrub.	//
	// It reads the metadata rather than the storage directory because the metadata is what knows
	// which blobs are SUPPOSED to exist — a directory scan verifies exactly the blobs that are
	// easy to find and misses a row whose bytes are gone.
	//
	// The cursor is what makes the scrub COVER the store. The previous signature returned the first
	// `limit` rows by insertion order, so a deployment with more blobs than the limit verified the
	// same oldest subset on every pass and never looked at anything newer — including freshly
	// written content, which is the most likely to be damaged. A zero `after` starts from the
	// beginning; the caller passes the last hash it received to continue, and starts over when a
	// pass returns fewer than `limit`.
	SampleBlobHashes(ctx context.Context, after [32]byte, limit int) ([][32]byte, error)

	// AuthorizeDownload opens or joins the download session for a shared file,
	// charging the share at most once per logical download.
	AuthorizeDownload(ctx context.Context, s DownloadSession, reuseExisting bool) (DownloadSession, bool, error)
	// ExpireDownloadSessions deletes sessions past their TTL.
	ExpireDownloadSessions(ctx context.Context) (int, error)

	// Recipient verification hardening: single-use codes and a per-address
	// failure window.
	OTPFailureCount(ctx context.Context, shareID uuid.UUID, email string, window time.Duration) (int, error)
	RecordOTPAttempt(ctx context.Context, a OTPAttempt) error
	ConsumeRecipientCode(ctx context.Context, shareID uuid.UUID, email string) error
	PruneOTPAttempts(ctx context.Context, olderThan time.Duration) (int, error)

	// Resource ACL (§4.1 layer 2): grants, group membership and the ancestor
	// walk that resolves them.
	Grant(ctx context.Context, e ACLEntry) (ACLEntry, error)
	Revoke(ctx context.Context, id uuid.UUID) error
	ACLForNode(ctx context.Context, nodeID uuid.UUID) ([]ACLEntry, error)
	EffectivePermsFor(ctx context.Context, actorID, nodeID uuid.UUID) (Perm, bool, error)
	// SharedWithMe lists nodes another account granted this actor access to.
	SharedWithMe(ctx context.Context, actorID uuid.UUID, limit int) ([]SharedNode, error)
	// Account administration: approving a JIT-provisioned account, disabling one, and
	// listing what is waiting for attention.
	ListUsers(ctx context.Context, status string, limit int) ([]User, error)
	SetUserStatus(ctx context.Context, id uuid.UUID, status string) (UserStatusChange, error)
	// DeleteUser applies the ID-08 deletion policy: files to the trash, shares and
	// credentials revoked, audit history kept, account tombstoned.
	DeleteUser(ctx context.Context, id uuid.UUID) (DeleteUserResult, error)

	// Grant-subject resolution, for the ACL management API: a caller names a
	// username or an address, never a UUID it would have to look up first.
	UserByUsername(ctx context.Context, username string) (User, error)
	UserByEmail(ctx context.Context, email string) (User, error)
	GroupByID(ctx context.Context, id uuid.UUID) (Group, error)
	CreateGroup(ctx context.Context, g Group) (Group, error)
	GroupByName(ctx context.Context, name string) (Group, error)
	// DeleteGroup removes a group. Memberships go with it and nodes that named it as owner keep
	// their content but stop being group spaces (ON DELETE SET NULL), so deleting a group never
	// deletes files.
	DeleteGroup(ctx context.Context, id uuid.UUID) error
	AddGroupMember(ctx context.Context, groupID, userID uuid.UUID, role string) error
	RemoveGroupMember(ctx context.Context, groupID, userID uuid.UUID) error
	GroupsForUser(ctx context.Context, userID uuid.UUID) ([]Group, error)
	// Groups lists every group, ordered by name: the admin screen needs to show what exists before
	// an operator can decide who belongs to it.
	Groups(ctx context.Context) ([]Group, error)
	GroupMembers(ctx context.Context, groupID uuid.UUID) ([]GroupMember, error)

	// Retention: permanent deletion of expired trash and versions.
	PurgeTrash(ctx context.Context, retention time.Duration) (PurgeResult, error)
	// PurgeTrashFor is the user-initiated "empty trash" (core-scope §3).
	//
	// It sits next to PurgeTrash rather than in a separate interface because the two MUST share
	// the accounting and the recursive delete: the acceptance criterion is that a concurrent purge
	// and delete cannot double-release a refcount, and that property is only true if there is one
	// implementation of the release. This is the same routine, scoped to one owner, ignoring
	// retention, and bounded so a huge bin does not hold one request open.
	PurgeTrashFor(ctx context.Context, ownerID uuid.UUID, maxRoots int) (PurgeResult, error)
	// ExpireRetained moves nodes whose share retention has lapsed into the trash.
	//
	// Into the trash, not out of the database: retention is a policy about how long deliveries
	// are kept, not an instruction to destroy them irrecoverably. The trash window still applies.
	ExpireRetained(ctx context.Context) (int, error)
	// ShareRateTx is the upload rate limit (SHARE-05 速率).
	ShareRateTx
	// SharePasswordTx is the password-guess throttle (SHARE-02 尝试上限).
	SharePasswordTx
	// SocialTx is favorites, tags and comments (core-scope §4, §5, §6).
	//
	// Embedded rather than folded into this block because the three arrived together and share the
	// node-id addressing rule; keeping them in their own file is what lets that rule be stated once.
	SocialTx
	ExpireVersions(ctx context.Context, retention time.Duration) (int, error)

	// Enqueue adds an asynchronous job in the same transaction.
	Enqueue(ctx context.Context, j Job) error

	// NodesAfter pages every node in id order, for the search backfill.
	//
	// Keyset on id rather than on updated_at: the id is immutable, so a walk that resumes after a
	// document was renamed or moved neither skips a row nor returns one twice. Trashed rows are
	// included, because the caller has to be able to tell "this document is gone" — it decides what
	// to do about that, and a walk that silently omitted them would leave deleted files in the index
	// forever.
	//
	// A nil uuid starts from the beginning; an empty result means the walk reached the end and the
	// next pass should start over.
	NodesAfter(ctx context.Context, after uuid.UUID, limit int) ([]Node, error)

	// OldestPendingJob reports when the oldest still-pending job of this kind was due, for the
	// search-index lag gauge (§7.2).
	//
	// Due time rather than creation time: a job scheduled a minute in the future is not late yet,
	// and a lag that counted it would report a backlog that is not one.
	//
	// ok is false when nothing of that kind is pending, which is the caught-up answer.
	OldestPendingJob(ctx context.Context, kind string) (due time.Time, ok bool, err error)

	// The consumer side of the queue (INV-06).
	ClaimJobs(ctx context.Context, workerID string, limit int, lease time.Duration) ([]QueuedJob, error)
	CompleteJob(ctx context.Context, id int64) error
	FailJob(ctx context.Context, id int64, cause error, permanent bool) (bool, error)
	ReapExpiredLeases(ctx context.Context) (int, error)
	JobCounts(ctx context.Context) (pending, running, failed int, err error)

	// Mounts: external storage pointed at a folder, never copied (ADR-0014).
	//
	// The mount row is a pointer: the mount-point folder is an ordinary nodes row and
	// nothing below it is ever stored. These methods manage the pointer, not the
	// contents — a provider answers for those, live.
	CreateMount(ctx context.Context, m NewMount) (Mount, error)
	// MountByNodeID answers "is this node a mount point, and what does it point at".
	//
	// ErrNotFound means it is an ordinary folder. The pairing is unique (mounts.node_id
	// is UNIQUE), so this is the one question both HTTP surfaces ask on every path that
	// resolves to a directory, and the answer is what decides whether resolution
	// continues in the database or hands the remaining segments to a provider.
	MountByNodeID(ctx context.Context, nodeID uuid.UUID) (Mount, error)
	MountByID(ctx context.Context, id uuid.UUID) (Mount, error)
	// MountsForOwner lists the mounts whose point lives in this owner's tree.
	MountsForOwner(ctx context.Context, ownerID uuid.UUID) ([]Mount, error)
	// AllMounts is the operator view. A mount dials outward with stored credentials;
	// the list of where it dials is exactly what an operator must be able to see.
	AllMounts(ctx context.Context) ([]Mount, error)
	// DeleteMount removes the mount row and its mount-point node in one transaction.
	//
	// The node is hard-deleted, not trashed: it is a pointer, not data, and a trashed
	// mount point would be a folder that resolves to a remote nobody can reach through
	// it. The remote's data is untouched — unmounting is not deletion, and the API says
	// so rather than implying it. Refuses a mount whose node grew live children, which
	// by construction cannot happen (nothing below a mount point is ever stored) and is
	// checked anyway because "by construction" is exactly what a later change erodes.
	DeleteMount(ctx context.Context, id uuid.UUID) error
	// SetMountHealth records the outcome of the last reachability check. It is
	// advisory — a stale "ok" must never authorize anything — but it is what the mount
	// list shows an operator instead of making them probe by hand.
	SetMountHealth(ctx context.Context, id uuid.UUID, status, detail string, at time.Time) error
}

// NewNode is the input for creating a directory.
type NewNode struct {
	OwnerID      uuid.UUID
	ParentID     uuid.UUID
	OwnerGroupID *uuid.UUID
	Name         string
}

// NewFile is the input for creating a file.
type NewFile struct {
	OwnerID      uuid.UUID
	ParentID     uuid.UUID
	OwnerGroupID *uuid.UUID
	Name         string
	Size         int64
	Mime         string
	ETag         string
	BlobID       uuid.UUID
	// BlobHash is the content address, needed to keep dedup accounting correct.
	BlobHash [32]byte
	// RetainUntil stamps the retention that applied when the file arrived (SHARE-05 保留期限).
	//
	// It is set from the share at delivery time rather than derived from the share later,
	// because the policy in force when the bytes arrived is the one the owner agreed to. A
	// link whose retention is shortened afterwards must not retroactively delete older
	// deliveries; lengthening it must not extend them either.
	RetainUntil *time.Time
	// ModifiedAt, when set, is the modification time the CLIENT asked for — the WebDAV
	// Nextcloud client sends it as X-OC-Mtime, and a sync client compares it against its
	// local clock. Nil means "now", which is the right answer for every other writer.
	//
	// It is deliberately not part of the ETag: the ETag identifies content, and an upload
	// that only re-stamps a timestamp is still the same bytes.
	ModifiedAt *time.Time
}

// UpdateFile replaces a file's content.
type UpdateFile struct {
	NodeID   uuid.UUID
	Size     int64
	Mime     string
	ETag     string
	BlobID   uuid.UUID
	BlobHash [32]byte
	// Expect is the caller's precondition on the current ETag. Nil means
	// unconditional, which is only correct for callers that have already
	// established there is no competing writer (a restore, for instance).
	Expect *Precondition
	// KeepVersion stores the previous content as a new version row. Callers
	// set this based on the retention policy and the storage layout (the tree
	// layout should keep fewer versions because each is a full copy).
	KeepVersion bool
	// MaxVersions bounds the kept versions; 0 means unlimited.
	MaxVersions int
	// By is the account performing the write, recorded on the version row this
	// write creates.
	//
	// §8.1 asks every version to carry "操作者引用" — a reference to the operator. The column
	// existed and was always written as NULL, so the history could say when a version was made and
	// what it contained, but never who made it. Nil is legitimate for background work, but a
	// user-initiated save must name the user.
	By *uuid.UUID
	// ModifiedAt, when set, replaces the server's clock for this write (X-OC-Mtime). See
	// NewFile.ModifiedAt.
	ModifiedAt *time.Time
}

// WriteFile is an upsert with a precondition: it creates the file when the name
// is free and replaces it when it is taken, all inside one transaction.
//
// Putting both cases in the store rather than in each handler is the point. The
// handler cannot do "look, decide, write" without racing, and the three handlers
// that needed this (browser upload, resumable upload, WebDAV PUT) had drifted
// apart ??two of them had no precondition at all.
type WriteFile struct {
	NewFile
	// Expect is checked against the existing sibling, if any, before anything is
	// written. Nil means unconditional.
	Expect *Precondition
	// KeepVersion keeps the replaced content as a version row.
	KeepVersion bool
	// MaxVersions bounds kept versions; 0 means unlimited.
	MaxVersions int
	// By is the account performing the write, recorded on the version row this
	// write creates. See UpdateFile.By.
	By *uuid.UUID
}

// Version is one historical revision of a file.
type Version struct {
	ID        uuid.UUID
	NodeID    uuid.UUID
	VersionNo int
	BlobID    uuid.UUID
	Size      int64
	// SHA256 is the content address of this version's bytes, joined from blobs.
	//
	// §8.1 requires every version to carry 内容校验信息 — a checksum — alongside its id and
	// creation time. The column was reachable all along through versions.blob_id, but no query
	// joined it, so the API listed a version's number, size and date with no way for a client to
	// verify the bytes it downloaded. A version you cannot check is a version you have to trust.
	SHA256    []byte
	CreatedAt time.Time
	CreatedBy *uuid.UUID
}

// NewMount is the input for recording an external mount (ADR-0014).
type NewMount struct {
	ID       uuid.UUID
	NodeID   uuid.UUID
	OwnerID  uuid.UUID
	Name     string
	Scheme   string
	Endpoint string
	// RootPath is the path inside the remote that the mount is rooted at, so one remote can
	// back several mounts without exposing all of itself to any one of them.
	RootPath string
	Username string
	// SecretEnc is the credential's AES-256-GCM ciphertext. The store never sees plaintext:
	// the cipher lives above it, keyed by the file from mounts.secret_file.
	SecretEnc []byte
	ReadOnly  bool
	CreatedBy uuid.UUID
}

// Mount is one recorded external mount. It is a pointer, not a copy (ADR-0014): the node is
// the folder users see, and every request below it asks the remote live.
type Mount struct {
	NewMount
	CreatedAt    time.Time
	HealthStatus string
	HealthDetail string
	HealthAt     *time.Time
}

// AuditEntry is a single audit record, mirrored from internal/audit.Event but
// without the chain fields: the hash chain is computed by the audit writer in
// the same transaction, so callers cannot forge prev_hash.
type AuditEntry struct {
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
	// RequestID correlates the event with the HTTP request that produced it (§10.1 request_id).
	//
	// Empty means the event did not come from a request — a background job, or a migration. Leaving
	// it explicit rather than generating a synthetic id keeps "this happened outside any request"
	// visible, which is itself worth knowing.
	RequestID string
	// AuthMethod is how the actor authenticated (§10.1): session, app_password, breakglass, oidc or
	// share_link. Empty means no authentication was involved.
	AuthMethod string
}

// Job is an asynchronous task queued with the business change.
// Job is a unit of deferred work.
//
// The stored row carries more state than this input shape — attempts, a lease, a
// terminal error — because those are the queue's bookkeeping rather than the caller's
// concern. Enqueue takes the four things a producer knows; the queue owns the rest.
type Job struct {
	Kind      string
	Payload   map[string]any
	DedupeKey string
	RunAfter  time.Time
	// MaxAttempts bounds retries. Zero means the default (five).
	//
	// It is on the input shape rather than hidden in the queue because "try this once"
	// is a real policy: a job whose failure is already recorded elsewhere should not
	// be retried five times over an hour to reach the same conclusion.
	MaxAttempts int
}

// queuedJob is one row of the in-memory queue, mirroring the columns postgres uses.
type queuedJob struct {
	ID          int64
	Kind        string
	Payload     map[string]any
	DedupeKey   string
	RunAfter    time.Time
	Attempts    int
	MaxAttempts int
	LastError   string
	LockedUntil *time.Time
	LockedBy    string
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// Store is the transaction entry point.
type Store interface {
	// WithTx runs fn inside a transaction, committing on nil and rolling back on error.
	//
	// Nesting is NOT supported, contrary to what this comment used to claim. Neither backend
	// reuses the outer transaction: PostgreSQL begins a second one — which cannot see the outer
	// transaction's uncommitted writes and can deadlock the pool — and the in-memory store refuses
	// it. Call the Tx methods you were given instead of opening another transaction inside the
	// callback, and pass THAT callback's ctx, not a fresh one: the in-memory re-entry guard keys
	// off the context, so a nested call carrying a different context waits for the outer
	// transaction to finish instead of being refused.
	WithTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error

	// Resolve walks a path from a user's root to a node. It returns ErrNotFound
	// for any missing or non-directory component.
	//
	// It returns a plain []Node rather than a path string so that callers work
	// with identities, not names.
	Resolve(ctx context.Context, ownerID uuid.UUID, path string) ([]Node, error)

	// PublishBlob commits the metadata half of a Blob-layout staged publish.
	// The content store has already written the immutable blob; this is the
	// conditional visibility decision. It is not used by handlers until Phase 1R.
	PublishBlob(ctx context.Context, req consistency.BlobPublish) error

	// AuditRows and AuditHead are the read side of the audit log, used by the anchor
	// verifier. A backend that keeps no sealed chain returns ErrNoChain from AuditHead.
	AuditRows(ctx context.Context, afterID int64, limit int) ([]audit.Event, error)
	AuditHead(ctx context.Context) ([]byte, error)

	// AuditCount and AuditNewest are the cheap health end of the log: how many
	// events there are, and when the newest one landed. They exist so monitoring
	// can publish the chain's length and head age without walking it — the head-age
	// gauge was once fed a literal zero, which made the alert on it decorative.
	AuditCount(ctx context.Context) (int64, error)
	AuditNewest(ctx context.Context) (time.Time, bool, error)

	// AllRoots returns every user's namespace root. Whole-deployment operations (the restore
	// drill) need it; per-request code should not.
	AllRoots(ctx context.Context) ([]Node, error)

	// Root returns the owner's root directory. It returns ErrNotFound when the
	// owner has never written anything.
	Root(ctx context.Context, ownerID uuid.UUID) (Node, error)

	// EnsureRoot returns the owner's root directory, creating it on first use.
	// It is idempotent and safe to call concurrently.
	EnsureRoot(ctx context.Context, ownerID uuid.UUID) (Node, error)

	// Usage returns the owner's accounted bytes.
	Usage(ctx context.Context, ownerID uuid.UUID) (int64, error)

	// Resolver exposes the blob-layout NodeResolver, so the same metadata
	// transaction serves both layouts.
	Resolver() Resolver

	// BlobIDFor maps a content address to the blobs row, registering it on first
	// use. The dedup index belongs to the metadata layer: the storage backend only
	// knows "these bytes have this hash", not who references them.
	// BlobIDFor returns the blobs row id for a content address, creating the row on first use.
	// size is the real content length: the row records it, and the GC reports reclaimed bytes
	// from it.
	BlobIDFor(ctx context.Context, hash [32]byte, size int64) uuid.UUID
}

// Resolver is the narrowed view the blob storage layout needs.
//
// It is declared here rather than in the storage package so the dependency runs
// storage -> store, never the reverse.
type Resolver interface {
	HashFor(ctx context.Context, owner, path string) ([32]byte, bool, error)
	Record(ctx context.Context, owner, path string, hash [32]byte, isDir bool) error
	Forget(ctx context.Context, owner, path string) error
	IsDir(ctx context.Context, owner, path string) (bool, bool, error)
	Children(ctx context.Context, owner, dirPath string, limit int) ([]ResolverNode, error)
}

// ResolverNode is the minimal shape the storage layer needs back.
type ResolverNode struct {
	Path  string
	IsDir bool
	Size  int64
}

// ErrNotImplemented reports that a backend does not model this concept at all.
//
// Distinct from an empty result on purpose. A caller that cannot tell "there is nothing here"
// from "this backend cannot answer" will report a clean result for work that never happened —
// which is exactly what happened to the blob collector: the in-memory store returned an empty
// list while its own comment claimed to report the gap, and the sweep reported success on a
// backend that never looked.
var ErrNotImplemented = fmt.Errorf("store: not implemented")

// errNotImplemented is kept as the internal spelling so existing call sites need no change.
var errNotImplemented = ErrNotImplemented

// ErrTrashFull is returned when a move to the trash would exceed the retained-usage cap.
var ErrTrashFull = errors.New("store: the trash is full")
