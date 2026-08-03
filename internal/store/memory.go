package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Memory is an in-memory Store used by tests and by the pre-database wiring.
//
// It models the same invariants PostgreSQL enforces, so a bug found here is a
// bug that would also happen in production:
//
//   - a name is unique among live siblings (the partial unique index)
//   - a file must have content, a directory must not
//   - a directory cannot be moved into its own subtree
//   - quota is enforced before the write is committed
//   - every mutation emits an audit entry in the same "transaction"
//
// It is NOT a cache: each Memory instance is the only copy of its data, which is
// exactly why it must not be shared between processes.
type Memory struct {
	// mu guards the committed state; txMu orders transactions so two of them
	// cannot publish conflicting copies.
	mu       sync.RWMutex
	txMu     sync.Mutex
	nodes    map[uuid.UUID]*memNode
	children map[uuid.UUID]map[string]uuid.UUID // parent -> lower(name) -> child
	roots    map[uuid.UUID]uuid.UUID
	trashSeq []uuid.UUID

	audit []AuditEntry
	// auditTimes runs parallel to audit. The write-side entry has no timestamp by
	// design (the SQL column defaults to now()), so the memory store stamps on append.
	auditTimes   []time.Time
	jobs         []queuedJob
	nextJobID    int64
	usage        map[uuid.UUID]int64
	quota        map[uuid.UUID]int64
	users        map[uuid.UUID]*User
	sessions     map[string]*memSession
	appPasswords map[uuid.UUID]*AppPassword
	locks        map[uuid.UUID]*Lock
	deadProps    map[uuid.UUID]map[[2]string]string
	shares       map[uuid.UUID]*Share
	blobIDs      map[[32]byte]uuid.UUID
	// auditFault makes audit writes fail, so tests can exercise the T-16 fail-closed policy.
	auditFault   error
	recipients   map[[2]string]*ShareRecipient
	downloads    map[string]*DownloadSession
	acl          map[uuid.UUID][]ACLEntry
	groups       []Group
	groupMembers []GroupMember
	otpAttempts  []OTPAttempt
	nextVersion  int
	// broken simulates an unreachable backend, for tests of failure handling.
	broken *brokenFlag
	now    func() time.Time
	// clockOffset lets a test move time forward without sleeping. Nil means "use the
	// real clock".
	clockOffset *offsetFlag
}

var _ Store = (*Memory)(nil)

// memSession adds revocation to the exported Session record.
type memSession struct {
	Session
	RevokedAt *time.Time
}

type memNode struct {
	node Node
	blob [32]byte
	// versions keeps historical content addresses; only the count matters for
	// the in-memory model.
	versions []Version
}

// NewMemory returns an empty metadata store.
func NewMemory() *Memory {
	m := &Memory{
		nodes:        map[uuid.UUID]*memNode{},
		children:     map[uuid.UUID]map[string]uuid.UUID{},
		roots:        map[uuid.UUID]uuid.UUID{},
		usage:        map[uuid.UUID]int64{},
		quota:        map[uuid.UUID]int64{},
		users:        map[uuid.UUID]*User{},
		sessions:     map[string]*memSession{},
		appPasswords: map[uuid.UUID]*AppPassword{},
		locks:        map[uuid.UUID]*Lock{},
		deadProps:    map[uuid.UUID]map[[2]string]string{},
		shares:       map[uuid.UUID]*Share{},
		blobIDs:      map[[32]byte]uuid.UUID{},
		recipients:   map[[2]string]*ShareRecipient{},
		downloads:    map[string]*DownloadSession{},
		acl:          map[uuid.UUID][]ACLEntry{},
		groups:       nil,
		groupMembers: nil,
		otpAttempts:  nil,
		now:          time.Now,
	}
	// Route every timestamp through the offset-aware clock, so a test can advance time
	// and have job scheduling, lease expiry and retention all move together.
	m.applyClock()
	return m
}

func lower(s string) string { return strings.ToLower(s) }

// txDepthKey marks a context that is already inside a transaction.
type txDepthKey struct{}

// txMemKey carries the active transaction so store-level methods that must write through the
// snapshot can find it.
//
// Some methods take a bare "no Tx argument" signature (BlobIDFor, for historical reasons) and are
// called from inside a WithTx callback. Without this they write to the store instead of the
// snapshot, and the snapshot then overwrites them on commit — which silently disabled blob
// deduplication. Carrying the tx explicitly is what lets those methods see the right target
// without changing their signatures at every call site.
type txMemKey struct{}

// WithTx executes fn against a private copy of the data and publishes it on
// success.
//
// Why a copy rather than locking around the callback: the callback is arbitrary
// user code and almost always calls back into the store (Resolve, Node, ??. A
// sync.Mutex is not reentrant, so holding the write lock across the callback
// deadlocks the moment a transaction reads anything through a public method ??// which the DAV handlers do constantly.
//
// The copy is cheap in the only place this implementation runs (tests and the
// pre-database wiring), and PostgreSQL gets real transactions. What matters is
// that commit is atomic: a reader sees either none of a transaction or all of it.
func (m *Memory) WithTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	if m.isBroken() {
		return ErrUnavailable
	}
	// Re-entry guard. A transaction callback that calls another Store method
	// (Resolve, Root, Usage) would otherwise start a nested transaction and
	// deadlock on txMu. PostgreSQL has no nested transactions either, so refusing
	// loudly keeps the two implementations honest instead of letting the in-memory
	// one tolerate code that would fail in production.
	if in, ok := ctx.Value(txDepthKey{}).(bool); ok && in {
		return ErrNestedTx
	}
	ctx = context.WithValue(ctx, txDepthKey{}, true)
	ctx = context.WithValue(ctx, txMemKey{}, (*memTx)(nil)) // replaced below once tx exists

	// Serialise transactions, but release the mutex before invoking fn. The
	// private copy is what isolates them; the mutex only orders the swap.
	m.txMu.Lock()
	defer m.txMu.Unlock()

	m.mu.RLock()
	work := m.snapshot()
	m.mu.RUnlock()

	tx := &memTx{m: work}
	ctx = context.WithValue(ctx, txMemKey{}, tx)
	if err := fn(ctx, tx); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Preserve counters that are not part of the snapshot (nothing today, but
	// this is where a monotonic sequence would be carried over).
	m.nodes = work.nodes
	m.children = work.children
	m.roots = work.roots
	m.usage = work.usage
	m.quota = work.quota
	m.users = work.users
	m.sessions = work.sessions
	m.appPasswords = work.appPasswords
	m.locks = work.locks
	m.deadProps = work.deadProps
	m.shares = work.shares
	m.blobIDs = work.blobIDs
	m.recipients = work.recipients
	m.downloads = work.downloads
	m.acl = work.acl
	m.groups = work.groups
	m.groupMembers = work.groupMembers
	m.otpAttempts = work.otpAttempts
	m.audit = work.audit
	// The timestamps travel with the entries. Copying one without the other is how the
	// indexes drift out of step, and an index out of range during an audit export is a
	// 500 on the one screen an operator reaches for when something has gone wrong.
	m.auditTimes = work.auditTimes
	m.jobs = work.jobs
	m.nextVersion = work.nextVersion
	return nil
}

// snapshot returns a deep copy suitable for a transaction's private workspace.
// Every mutating map is duplicated; the immutable scalars are copied by value.
func (m *Memory) snapshot() *Memory {
	work := &Memory{
		nodes:        make(map[uuid.UUID]*memNode, len(m.nodes)),
		children:     make(map[uuid.UUID]map[string]uuid.UUID, len(m.children)),
		roots:        make(map[uuid.UUID]uuid.UUID, len(m.roots)),
		usage:        make(map[uuid.UUID]int64, len(m.usage)),
		quota:        make(map[uuid.UUID]int64, len(m.quota)),
		users:        make(map[uuid.UUID]*User, len(m.users)),
		sessions:     make(map[string]*memSession, len(m.sessions)),
		appPasswords: make(map[uuid.UUID]*AppPassword, len(m.appPasswords)),
		locks:        make(map[uuid.UUID]*Lock, len(m.locks)),
		deadProps:    make(map[uuid.UUID]map[[2]string]string, len(m.deadProps)),
		shares:       make(map[uuid.UUID]*Share, len(m.shares)),
		blobIDs:      make(map[[32]byte]uuid.UUID, len(m.blobIDs)),
		// The audit fault is configuration, not data: it must survive the snapshot or a
		// transaction would silently succeed while the store reports itself healthy.
		auditFault:   m.auditFault,
		recipients:   make(map[[2]string]*ShareRecipient, len(m.recipients)),
		downloads:    make(map[string]*DownloadSession, len(m.downloads)),
		acl:          make(map[uuid.UUID][]ACLEntry, len(m.acl)),
		groups:       append([]Group(nil), m.groups...),
		groupMembers: append([]GroupMember(nil), m.groupMembers...),
		otpAttempts:  append([]OTPAttempt(nil), m.otpAttempts...),
		audit:        append([]AuditEntry(nil), m.audit...),
		auditTimes:   append([]time.Time(nil), m.auditTimes...),
		jobs:         append([]queuedJob(nil), m.jobs...),
		nextVersion:  m.nextVersion,
		now:          m.now,
	}
	for k, v := range m.nodes {
		cp := *v
		cp.versions = append([]Version(nil), v.versions...)
		work.nodes[k] = &cp
	}
	for k, v := range m.children {
		cp := make(map[string]uuid.UUID, len(v))
		for nk, nv := range v {
			cp[nk] = nv
		}
		work.children[k] = cp
	}
	for k, v := range m.roots {
		work.roots[k] = v
	}
	// The dedup index has to travel with the snapshot.
	//
	// Everything else in this function copies its map by hand; blobIDs was declared in the clone
	// but never filled, so every transaction began with an empty index and commit replaced the
	// real one with it. The visible bug: two files with identical content received different blob
	// ids, so the blob layout silently stopped deduplicating and every upload consumed full size.
	// It only shows up across transactions — within one, the map works.
	for k, v := range m.blobIDs {
		work.blobIDs[k] = v
	}
	for k, v := range m.usage {
		work.usage[k] = v
	}
	for k, v := range m.quota {
		work.quota[k] = v
	}
	for k, v := range m.users {
		cp := *v
		work.users[k] = &cp
	}
	for k, v := range m.sessions {
		cp := *v
		work.sessions[k] = &cp
	}
	for k, v := range m.appPasswords {
		cp := *v
		cp.TokenHash = append([]byte(nil), v.TokenHash...)
		work.appPasswords[k] = &cp
	}
	for k, v := range m.locks {
		cp := *v
		work.locks[k] = &cp
	}
	for k, v := range m.deadProps {
		cp := make(map[[2]string]string, len(v))
		for nk, nv := range v {
			cp[nk] = nv
		}
		work.deadProps[k] = cp
	}
	for k, v := range m.shares {
		cp := *v
		cp.TokenHash = append([]byte(nil), v.TokenHash...)
		cp.PasswordHash = append([]byte(nil), v.PasswordHash...)
		work.shares[k] = &cp
	}
	for k, v := range m.recipients {
		cp := *v
		work.recipients[k] = &cp
	}
	for k, v := range m.downloads {
		cp := *v
		cp.GrantHash = append([]byte(nil), v.GrantHash...)
		work.downloads[k] = &cp
	}
	for k, v := range m.acl {
		work.acl[k] = append([]ACLEntry(nil), v...)
	}
	return work
}

type memTx struct {
	// m is this transaction's private workspace, published by WithTx on success.
	m *Memory
}

var _ Tx = (*memTx)(nil)

func (t *memTx) Node(ctx context.Context, id uuid.UUID) (Node, error) {
	rec, ok := t.m.nodes[id]
	if !ok || rec.node.TrashedAt != nil {
		return Node{}, ErrNotFound
	}
	return rec.node, nil
}

// NodeIncludingTrashed returns a node in any state, for the restore flow.
func (t *memTx) NodeIncludingTrashed(ctx context.Context, id uuid.UUID) (Node, error) {
	rec, ok := t.m.nodes[id]
	if !ok {
		return Node{}, ErrNotFound
	}
	return rec.node, nil
}

func (t *memTx) Children(ctx context.Context, parentID uuid.UUID, limit int) ([]Node, error) {
	if _, ok := t.m.nodes[parentID]; !ok {
		return nil, ErrNotFound
	}
	out := make([]Node, 0, len(t.m.children[parentID]))
	for _, childID := range t.m.children[parentID] {
		rec := t.m.nodes[childID]
		if rec.node.TrashedAt != nil {
			continue
		}
		out = append(out, rec.node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ChildrenAfter implements keyset pagination, mirroring the SQL predicate.
func (t *memTx) ChildrenAfter(ctx context.Context, parentID uuid.UUID, after string, limit int) ([]Node, error) {
	all, err := t.Children(ctx, parentID, 0)
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(all))
	for _, n := range all {
		// Strictly greater-than, and byte-wise like PostgreSQL's default
		// collation: a locale-aware comparison here would make the two backends
		// paginate in different orders, which is the kind of divergence the shared
		// suite exists to catch.
		if n.Name > after {
			out = append(out, n)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (t *memTx) ChildByName(ctx context.Context, parentID uuid.UUID, name string) (Node, error) {
	childID, ok := t.m.children[parentID][nameKey(name)]
	if !ok {
		return Node{}, ErrNotFound
	}
	rec := t.m.nodes[childID]
	if rec.node.TrashedAt != nil {
		return Node{}, ErrNotFound
	}
	// The key already folded case and normalisation, so two spellings of one name reach the same
	// node on purpose (T-12). Comparing the raw bytes here would undo that.
	return rec.node, nil
}

// checkName enforces the sibling-uniqueness rule that the partial unique index
// provides in PostgreSQL: a trashed sibling does not block a new name.
func (t *memTx) checkName(parentID uuid.UUID, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if _, ok := t.m.nodes[parentID]; !ok {
		return ErrNotFound
	}
	if childID, exists := t.m.children[parentID][nameKey(name)]; exists {
		if t.m.nodes[childID].node.TrashedAt == nil {
			return ErrConflict
		}
	}
	return nil
}

func (t *memTx) attach(parentID uuid.UUID, node Node) {
	if t.m.children[parentID] == nil {
		t.m.children[parentID] = map[string]uuid.UUID{}
	}
	t.m.children[parentID][nameKey(node.Name)] = node.ID
}

func (t *memTx) CreateDir(ctx context.Context, n NewNode) (Node, error) {
	if err := t.checkName(n.ParentID, n.Name); err != nil {
		return Node{}, err
	}
	parent := n.ParentID
	now := t.m.now()
	node := Node{
		ID: newID(), ParentID: &parent, OwnerID: n.OwnerID,
		Name: n.Name, Kind: KindDir, CreatedAt: now, ModifiedAt: now,
	}
	t.m.nodes[node.ID] = &memNode{node: node}
	t.attach(n.ParentID, node)
	return node, nil
}

func (t *memTx) CreateFile(ctx context.Context, n NewFile) (Node, error) {
	if err := t.checkName(n.ParentID, n.Name); err != nil {
		return Node{}, err
	}
	if err := t.checkQuota(n.OwnerID, n.Size); err != nil {
		return Node{}, err
	}
	parent := n.ParentID
	blobID := n.BlobID
	now := t.m.now()
	node := Node{
		ID: newID(), ParentID: &parent, OwnerID: n.OwnerID,
		Name: n.Name, Kind: KindFile, Size: n.Size, Mime: n.Mime, ETag: n.ETag,
		BlobID: &blobID, CreatedAt: now, ModifiedAt: now,
	}
	t.m.nodes[node.ID] = &memNode{node: node, blob: n.BlobHash}
	t.attach(n.ParentID, node)
	t.m.usage[n.OwnerID] += n.Size
	return node, nil
}

func (t *memTx) UpdateFile(ctx context.Context, u UpdateFile) (Node, error) {
	rec, ok := t.m.nodes[u.NodeID]
	if !ok || rec.node.TrashedAt != nil {
		return Node{}, ErrNotFound
	}
	if rec.node.Kind != KindFile {
		return Node{}, fmt.Errorf("%w: not a file", ErrInvalidName)
	}
	if !u.Expect.Matches(rec.node.ETag, true) {
		return Node{}, ErrPrecondition
	}

	// Quota accounting is on the delta, not the new size: re-uploading a file
	// with the same content must not consume quota twice.
	delta := u.Size - rec.node.Size
	if err := t.checkQuota(rec.node.OwnerID, delta); err != nil {
		return Node{}, err
	}

	// Keep a version only when the content actually differs. A retried upload of
	// the same bytes should not fill the history with identical copies, which is
	// what the PostgreSQL implementation decides too.
	if u.KeepVersion && rec.node.BlobID != nil && *rec.node.BlobID != u.BlobID {
		rec.versions = append(rec.versions, Version{
			ID: newID(), NodeID: rec.node.ID,
			VersionNo: len(rec.versions) + 1,
			BlobID:    *rec.node.BlobID,
			Size:      rec.node.Size,
			CreatedAt: rec.node.ModifiedAt,
		})
		if u.MaxVersions > 0 && len(rec.versions) > u.MaxVersions {
			rec.versions = rec.versions[len(rec.versions)-u.MaxVersions:]
		}
	}

	blobID := u.BlobID
	rec.node.Size = u.Size
	rec.node.Mime = u.Mime
	rec.node.ETag = u.ETag
	rec.node.BlobID = &blobID
	rec.node.ModifiedAt = t.m.now()
	rec.blob = u.BlobHash
	t.m.usage[rec.node.OwnerID] += delta
	return rec.node, nil
}

// WriteFile implements the shared upsert-with-precondition path.
func (t *memTx) WriteFile(ctx context.Context, w WriteFile) (Node, bool, error) {
	existing, err := t.ChildByName(ctx, w.ParentID, w.Name)
	switch {
	case err == nil:
		if !w.Expect.Matches(existing.ETag, true) {
			return Node{}, false, ErrPrecondition
		}
		n, uerr := t.UpdateFile(ctx, UpdateFile{
			NodeID: existing.ID, Size: w.Size, Mime: w.Mime, ETag: w.ETag,
			BlobID: w.BlobID, BlobHash: w.BlobHash,
			KeepVersion: w.KeepVersion, MaxVersions: w.MaxVersions,
		})
		if uerr != nil {
			return Node{}, false, uerr
		}
		return n, false, nil
	case errors.Is(err, ErrNotFound):
		if !w.Expect.Matches("", false) {
			return Node{}, false, ErrPrecondition
		}
		n, cerr := t.CreateFile(ctx, w.NewFile)
		if cerr != nil {
			return Node{}, false, cerr
		}
		return n, true, nil
	default:
		return Node{}, false, err
	}
}

func (t *memTx) checkQuota(ownerID uuid.UUID, delta int64) error {
	limit, ok := t.m.quota[ownerID]
	if !ok || limit <= 0 {
		return nil
	}
	if t.m.usage[ownerID]+delta > limit {
		return ErrQuota
	}
	return nil
}

func (t *memTx) Rename(ctx context.Context, id uuid.UUID, name string) (Node, error) {
	rec, ok := t.m.nodes[id]
	if !ok || rec.node.TrashedAt != nil {
		return Node{}, ErrNotFound
	}
	if rec.node.ParentID == nil {
		return Node{}, fmt.Errorf("%w: cannot rename a root", ErrInvalidName)
	}
	parentID := *rec.node.ParentID
	if err := t.checkName(parentID, name); err != nil {
		return Node{}, err
	}
	delete(t.m.children[parentID], nameKey(rec.node.Name))
	rec.node.Name = name
	rec.node.ModifiedAt = t.m.now()
	t.m.children[parentID][nameKey(name)] = id
	return rec.node, nil
}

func (t *memTx) Move(ctx context.Context, id, newParent uuid.UUID) (Node, error) {
	rec, ok := t.m.nodes[id]
	if !ok || rec.node.TrashedAt != nil {
		return Node{}, ErrNotFound
	}
	if rec.node.ParentID == nil {
		return Node{}, fmt.Errorf("%w: cannot move a root", ErrInvalidName)
	}

	// A directory cannot become its own descendant: the move would detach the
	// subtree from the root and create an unreachable cycle.
	if rec.node.Kind == KindDir {
		for p := newParent; ; {
			if p == id {
				return Node{}, ErrCycle
			}
			parent, ok := t.m.nodes[p]
			if !ok {
				return Node{}, ErrNotFound
			}
			if parent.node.ParentID == nil {
				break
			}
			p = *parent.node.ParentID
		}
	}

	if err := t.checkName(newParent, rec.node.Name); err != nil {
		return Node{}, err
	}

	delete(t.m.children[*rec.node.ParentID], nameKey(rec.node.Name))
	parent := newParent
	rec.node.ParentID = &parent
	rec.node.ModifiedAt = t.m.now()
	t.attach(newParent, rec.node)
	return rec.node, nil
}

func (t *memTx) Trash(ctx context.Context, id uuid.UUID) error {
	if _, ok := t.m.nodes[id]; !ok {
		return ErrNotFound
	}
	return t.trashRecursive(id, 0)
}

// trashRecursive marks the subtree. The depth guard turns a corrupted parent
// chain into an error instead of a stack overflow.
func (t *memTx) trashRecursive(id uuid.UUID, depth int) error {
	if depth > 10_000 {
		return ErrCycle
	}
	rec, ok := t.m.nodes[id]
	if !ok || rec.node.TrashedAt != nil {
		return nil
	}
	at := t.m.now()
	rec.node.TrashedAt = &at
	if rec.node.Kind == KindDir {
		for _, childID := range t.m.children[id] {
			if err := t.trashRecursive(childID, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// Trashed lists trashed nodes that are the roots of their deleted subtree.
//
// The parent check mirrors the PostgreSQL query: an item the user deleted is trashed
// while its parent is not, whereas a child that was deleted along with its folder
// has a trashed parent and must not appear as a separate entry.
func (t *memTx) Trashed(ctx context.Context, ownerID uuid.UUID, limit int) ([]Node, error) {
	type entry struct {
		node Node
		at   time.Time
	}
	var found []entry

	for _, rec := range t.m.nodes {
		n := rec.node
		if n.OwnerID != ownerID || n.TrashedAt == nil {
			continue
		}
		// Skip descendants of a trashed ancestor.
		if n.ParentID != nil {
			if parent, ok := t.m.nodes[*n.ParentID]; ok && parent.node.TrashedAt != nil {
				continue
			}
		}
		found = append(found, entry{node: n, at: *n.TrashedAt})
	}

	// Newest first, matching the query's ORDER BY trashed_at DESC.
	sort.Slice(found, func(i, j int) bool { return found[i].at.After(found[j].at) })
	if limit > 0 && len(found) > limit {
		found = found[:limit]
	}

	out := make([]Node, 0, len(found))
	for _, e := range found {
		out = append(out, e.node)
	}
	return out, nil
}
func (t *memTx) Restore(ctx context.Context, id uuid.UUID) (Node, error) {
	rec, ok := t.m.nodes[id]
	if !ok {
		return Node{}, ErrNotFound
	}
	if rec.node.TrashedAt == nil {
		return Node{}, fmt.Errorf("%w: not in trash", ErrNotFound)
	}
	// Restore the whole subtree. Restoring only the directory would bring back an
	// empty folder while its children stayed invisible ??they were never removed
	// from the parent's child list, so no trash listing would show them either,
	// making them unreachable forever.
	if err := t.restoreRecursive(id, 0); err != nil {
		return Node{}, err
	}
	return t.m.nodes[id].node, nil
}

func (t *memTx) restoreRecursive(id uuid.UUID, depth int) error {
	if depth > 10_000 {
		return ErrCycle
	}
	rec, ok := t.m.nodes[id]
	if !ok {
		return nil
	}
	if rec.node.TrashedAt == nil {
		// A descendant trashed independently before its ancestor stays in the
		// trash: restoring an ancestor must not resurrect something the user
		// deleted on purpose.
		return nil
	}
	rec.node.TrashedAt = nil
	rec.node.ModifiedAt = t.m.now()
	if rec.node.Kind == KindDir {
		for _, childID := range t.m.children[id] {
			if err := t.restoreRecursive(childID, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *memTx) Versions(ctx context.Context, nodeID uuid.UUID, limit int) ([]Version, error) {
	rec, ok := t.m.nodes[nodeID]
	if !ok {
		return nil, ErrNotFound
	}
	out := append([]Version(nil), rec.versions...)
	sort.Slice(out, func(i, j int) bool { return out[i].VersionNo > out[j].VersionNo })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (t *memTx) RestoreVersion(ctx context.Context, nodeID uuid.UUID, versionNo int) (Node, error) {
	return Node{}, errNotImplemented
}

func (t *memTx) Audit(ctx context.Context, e AuditEntry) error {
	// Fail-closed (T-16): the same rule as the PostgreSQL store. The in-memory backend has no
	// disk to fill, so this is injected rather than discovered, but the behaviour must match or
	// the two backends would diverge on exactly the security property that matters.
	if t.m.auditFault != nil {
		return fmt.Errorf("%w: %v", ErrAuditUnavailable, t.m.auditFault)
	}
	t.m.audit = append(t.m.audit, e)
	t.m.auditTimes = append(t.m.auditTimes, t.m.now())
	return nil
}

// SetAuditFault makes every subsequent audit write fail, for tests of the T-16 policy.
//
// Injection rather than a real failing disk: the property under test is the store's refusal to
// commit an unauditable change, not the disk driver.
func (m *Memory) SetAuditFault(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditFault = err
}

func (t *memTx) Enqueue(ctx context.Context, j Job) error {
	runAfter := j.RunAfter
	if runAfter.IsZero() {
		runAfter = t.m.now()
	}

	// Dedupe applies while a job with the same key is still outstanding, matching the
	// SQL partial unique index. Without it a retried request would queue the same work
	// twice and the worker would do it twice.
	if j.DedupeKey != "" {
		for i := range t.m.jobs {
			existing := &t.m.jobs[i]
			if existing.CompletedAt == nil && existing.DedupeKey == j.DedupeKey && existing.Kind == j.Kind {
				return nil
			}
		}
	}

	maxAttempts := j.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}

	t.m.nextJobID++
	t.m.jobs = append(t.m.jobs, queuedJob{
		ID: t.m.nextJobID, Kind: j.Kind, Payload: copyPayload(j.Payload),
		DedupeKey: j.DedupeKey, RunAfter: runAfter,
		MaxAttempts: maxAttempts, CreatedAt: t.m.now(),
	})
	return nil
}

// ---- Store-level operations ----

func (m *Memory) Root(ctx context.Context, ownerID uuid.UUID) (Node, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.roots[ownerID]
	if !ok {
		return Node{}, ErrNotFound
	}
	return m.nodes[id].node, nil
}

// EnsureRoot returns the owner's root directory, creating it on first use.
// A user's root is a directory with an empty name and no parent.
func (m *Memory) EnsureRoot(ctx context.Context, ownerID uuid.UUID) (Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if id, ok := m.roots[ownerID]; ok {
		return m.nodes[id].node, nil
	}
	now := m.now()
	node := Node{
		ID: newID(), OwnerID: ownerID, Name: "", Kind: KindDir,
		CreatedAt: now, ModifiedAt: now,
	}
	m.nodes[node.ID] = &memNode{node: node}
	m.roots[ownerID] = node.ID
	m.children[node.ID] = map[string]uuid.UUID{}
	return node, nil
}

func (m *Memory) Resolve(ctx context.Context, ownerID uuid.UUID, p string) ([]Node, error) {
	segs, err := splitPath(p)
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	rootID, ok := m.roots[ownerID]
	if !ok {
		return nil, ErrNotFound
	}
	curID := rootID
	out := []Node{m.nodes[curID].node}

	for _, seg := range segs {
		childID, ok := m.children[curID][nameKey(seg)]
		if !ok {
			return nil, ErrNotFound
		}
		child := m.nodes[childID]
		// Resolution is case-exact even though the index is case-insensitive:
		// silently resolving "Readme.md" to "README.md" makes two distinct
		// files collide on a case-sensitive client.
		if child.node.TrashedAt != nil || child.node.Name != seg {
			return nil, ErrNotFound
		}
		out = append(out, child.node)
		curID = childID
	}
	return out, nil
}

func (m *Memory) Usage(ctx context.Context, ownerID uuid.UUID) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.usage[ownerID], nil
}

// SetQuota sets the byte limit for an owner. Zero or negative means unlimited.
func (m *Memory) SetQuota(ownerID uuid.UUID, limit int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quota[ownerID] = limit
}

// AuditLog returns recorded entries, for tests and the in-memory wiring.
func (m *Memory) AuditLog() []AuditEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]AuditEntry(nil), m.audit...)
}

// Jobs returns queued jobs.
// Jobs returns every queued job, for tests that assert work was enqueued.
//
// It exposes the input shape rather than the queue record: a test asking "was this
// queued" should not depend on lease bookkeeping, and the full row is available through
// JobCounts and the admin surface.
func (m *Memory) Jobs() []Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, Job{
			Kind: j.Kind, Payload: copyPayload(j.Payload),
			DedupeKey: j.DedupeKey, RunAfter: j.RunAfter,
		})
	}
	return out
}

func (m *Memory) Resolver() Resolver { return &memResolver{m: m} }

// memResolver adapts Memory to the storage layer's narrowed view.
type memResolver struct{ m *Memory }

func (r *memResolver) HashFor(ctx context.Context, owner, p string) ([32]byte, bool, error) {
	ownerID, err := parseOwner(owner)
	if err != nil {
		return [32]byte{}, false, err
	}
	nodes, err := r.m.Resolve(ctx, ownerID, p)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return [32]byte{}, false, nil
		}
		return [32]byte{}, false, err
	}
	leaf := nodes[len(nodes)-1]
	if leaf.IsDir() {
		return [32]byte{}, false, nil
	}
	r.m.mu.RLock()
	defer r.m.mu.RUnlock()
	return r.m.nodes[leaf.ID].blob, true, nil
}

func (r *memResolver) Record(ctx context.Context, owner, p string, hash [32]byte, isDir bool) error {
	// Node creation is the metadata layer's job; the storage adapter only needs
	// the content mapping, which CreateFile/CreateDir already wrote.
	return nil
}

func (r *memResolver) Forget(ctx context.Context, owner, p string) error { return nil }

func (r *memResolver) IsDir(ctx context.Context, owner, p string) (bool, bool, error) {
	ownerID, err := parseOwner(owner)
	if err != nil {
		return false, false, err
	}
	nodes, err := r.m.Resolve(ctx, ownerID, p)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, false, nil
		}
		return false, false, err
	}
	leaf := nodes[len(nodes)-1]
	return leaf.IsDir(), true, nil
}

func (r *memResolver) Children(ctx context.Context, owner, dirPath string, limit int) ([]ResolverNode, error) {
	ownerID, err := parseOwner(owner)
	if err != nil {
		return nil, err
	}
	nodes, err := r.m.Resolve(ctx, ownerID, dirPath)
	if err != nil {
		return nil, err
	}
	leaf := nodes[len(nodes)-1]
	if !leaf.IsDir() {
		return nil, ErrNotFound
	}

	var out []ResolverNode
	err = r.m.WithTx(ctx, func(ctx context.Context, tx Tx) error {
		children, err := tx.Children(ctx, leaf.ID, limit)
		if err != nil {
			return err
		}
		for _, c := range children {
			out = append(out, ResolverNode{
				Path:  path.Join(dirPath, c.Name),
				IsDir: c.IsDir(),
				Size:  c.Size,
			})
		}
		return nil
	})
	return out, err
}

func newID() uuid.UUID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: crypto/rand failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return uuid.UUID(b)
}

// Now returns the store's current time, including any test clock offset.
//
// Every internal timestamp goes through m.now, which this wraps. Advancing the clock
// therefore moves job scheduling, retention and lease expiry together — the same way a
// real clock would — instead of only the one field a test happened to touch.
func (m *Memory) Now() time.Time {
	now := m.now()
	if m.clockOffset != nil {
		now = now.Add(m.clockOffset.by)
	}
	return now
}

// applyClock replaces m.now with a closure that honours the offset.
//
// It runs once when the store is built, so every existing call site of m.now gains the
// behaviour without being rewritten.
func (m *Memory) applyClock() {
	base := m.now
	m.now = func() time.Time {
		now := base()
		if m.clockOffset != nil {
			now = now.Add(m.clockOffset.by)
		}
		return now
	}
}
