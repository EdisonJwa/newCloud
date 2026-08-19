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
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/audit"
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

	// Versions lists kept versions of a file, newest first.
	Versions(ctx context.Context, nodeID uuid.UUID, limit int) ([]Version, error)
	// RestoreVersion makes an old version current, keeping the current one.
	RestoreVersion(ctx context.Context, nodeID uuid.UUID, versionNo int) (Node, error)

	// Audit appends an event to the audit log in the same transaction.
	Audit(ctx context.Context, e AuditEntry) error

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

	// UserByID and UserByIdPSubject are the identity reads used during login.
	UserByID(ctx context.Context, id uuid.UUID) (User, error)
	UserByIdPSubject(ctx context.Context, issuer, subject string) (User, error)
	// UpsertIdPUser provisions or refreshes an account from IdP claims.
	UpsertIdPUser(ctx context.Context, u UpsertUser) (User, error)
	// ApplyIdPRole records the role the IdP now claims and revokes live
	// credentials on a downgrade.
	ApplyIdPRole(ctx context.Context, userID uuid.UUID, role string) (RoleChange, error)

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

	// WebDAV lock operations. Locks are durable because an in-memory lock system
	// breaks under restart or a second process, silently corrupting co-edited
	// documents.
	CreateLock(ctx context.Context, l Lock) error
	ActiveLock(ctx context.Context, nodeID uuid.UUID) (Lock, error)
	RefreshLock(ctx context.Context, nodeID uuid.UUID, token string, userID uuid.UUID, expires time.Time) (Lock, error)
	DeleteLock(ctx context.Context, nodeID uuid.UUID, token string, userID uuid.UUID) (bool, error)
	ExpireLocks(ctx context.Context) (int, error)
	LocksForUser(ctx context.Context, userID uuid.UUID) ([]Lock, error)

	// WebDAV dead properties. Windows Explorer and macOS Finder store per-file
	// metadata here and misbehave when it does not survive a reconnect.
	SetDeadProp(ctx context.Context, nodeID uuid.UUID, space, name, value string) error
	DeadProps(ctx context.Context, nodeID uuid.UUID) ([]DeadProp, error)
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
	// SampleBlobHashes returns up to limit content addresses, for an integrity scrub.
	//
	// It reads the metadata rather than the storage directory because the metadata is what knows
	// which blobs are SUPPOSED to exist — a directory scan verifies exactly the blobs that are
	// easy to find and misses a row whose bytes are gone.
	SampleBlobHashes(ctx context.Context, limit int) ([][32]byte, error)

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
	AddGroupMember(ctx context.Context, groupID, userID uuid.UUID, role string) error
	RemoveGroupMember(ctx context.Context, groupID, userID uuid.UUID) error
	GroupsForUser(ctx context.Context, userID uuid.UUID) ([]Group, error)
	GroupMembers(ctx context.Context, groupID uuid.UUID) ([]GroupMember, error)

	// Retention: permanent deletion of expired trash and versions.
	PurgeTrash(ctx context.Context, retention time.Duration) (PurgeResult, error)
	// ExpireRetained moves nodes whose share retention has lapsed into the trash.
	//
	// Into the trash, not out of the database: retention is a policy about how long deliveries
	// are kept, not an instruction to destroy them irrecoverably. The trash window still applies.
	ExpireRetained(ctx context.Context) (int, error)
	// ShareRateTx is the upload rate limit (SHARE-05 速率).
	ShareRateTx
	ExpireVersions(ctx context.Context, retention time.Duration) (int, error)

	// Enqueue adds an asynchronous job in the same transaction.
	Enqueue(ctx context.Context, j Job) error

	// The consumer side of the queue (INV-06).
	ClaimJobs(ctx context.Context, workerID string, limit int, lease time.Duration) ([]QueuedJob, error)
	CompleteJob(ctx context.Context, id int64) error
	FailJob(ctx context.Context, id int64, cause error, permanent bool) (bool, error)
	ReapExpiredLeases(ctx context.Context) (int, error)
	JobCounts(ctx context.Context) (pending, running, failed int, err error)
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
}

// Version is one historical revision of a file.
type Version struct {
	ID        uuid.UUID
	NodeID    uuid.UUID
	VersionNo int
	BlobID    uuid.UUID
	Size      int64
	CreatedAt time.Time
	CreatedBy *uuid.UUID
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
	// WithTx runs fn inside a transaction, committing on nil and rolling back
	// on error. Nested calls reuse the outer transaction.
	WithTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error

	// Resolve walks a path from a user's root to a node. It returns ErrNotFound
	// for any missing or non-directory component.
	//
	// It returns a plain []Node rather than a path string so that callers work
	// with identities, not names.
	Resolve(ctx context.Context, ownerID uuid.UUID, path string) ([]Node, error)

	// AuditRows and AuditHead are the read side of the audit log, used by the anchor
	// verifier. A backend that keeps no sealed chain returns ErrNoChain from AuditHead.
	AuditRows(ctx context.Context, afterID int64, limit int) ([]audit.Event, error)
	AuditHead(ctx context.Context) ([]byte, error)

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

var errNotImplemented = fmt.Errorf("store: not implemented")
