package store

import (
	"context"

	"github.com/google/uuid"
	"time"
)

// In-memory implementation of LifecycleTx.
//
// It follows the same order as the SQL version — account, then delete — so the two
// cannot drift into different quota or refcount behaviour.

func (t *memTx) PurgeTrash(ctx context.Context, retention time.Duration) (PurgeResult, error) {
	var res PurgeResult
	if retention <= 0 {
		return res, nil
	}

	cutoff := t.m.now().Add(-retention)

	// Eligible roots, collected first: deleting while iterating the map would make
	// the walk see a tree that is changing underneath it.
	var roots []PurgeableTrashRoot
	for _, rec := range t.m.nodes {
		n := rec.node
		if n.TrashedAt == nil || n.TrashedAt.After(cutoff) {
			continue
		}
		if n.ParentID != nil {
			if parent, ok := t.m.nodes[*n.ParentID]; ok && parent.node.TrashedAt != nil {
				continue
			}
		}
		roots = append(roots, PurgeableTrashRoot{
			NodeID: n.ID, OwnerID: n.OwnerID, Name: n.Name, TrashedAt: *n.TrashedAt,
		})
	}

	for _, root := range roots {
		nodes, bytes := t.purgeMemory(root.NodeID)
		res.Nodes += nodes
		res.Bytes += bytes
		res.Roots++
	}
	return res, nil
}

// purgeMemory removes one subtree and returns how many nodes and bytes it released.
func (t *memTx) purgeMemory(id [16]byte) (nodes int, bytes int64) {
	ids := t.collectSubtree(id)

	for _, nid := range ids {
		rec, ok := t.m.nodes[nid]
		if !ok {
			continue
		}
		bytes += rec.node.Size
		if rec.node.Size > 0 {
			// A purged node is always trashed, and trashing already released its bytes from the
			// logical quota. Subtracting from usage here would subtract them twice and hand the
			// account free quota it never had; the bytes leave RETAINED usage instead.
			//
			// Clamped at zero like the quota: an accounting bug must not produce a negative counter
			// that silently grants unlimited storage.
			if t.m.trashBytes == nil {
				t.m.trashBytes = map[uuid.UUID]int64{}
			}
			if t.m.trashBytes[rec.node.OwnerID] >= rec.node.Size {
				t.m.trashBytes[rec.node.OwnerID] -= rec.node.Size
			} else {
				t.m.trashBytes[rec.node.OwnerID] = 0
			}
		}
		// Versions die with the node; their content is no longer referenced.
		for range rec.versions {
			// The in-memory model does not refcount blobs, so there is nothing to
			// release here — see the comment in CollectableBlobs.
		}
		if rec.node.ParentID != nil {
			delete(t.m.children[*rec.node.ParentID], nameKey(rec.node.Name))
		}
		delete(t.m.nodes, nid)
		delete(t.m.children, nid)
		nodes++
	}
	return nodes, bytes
}

// collectSubtree lists id and every descendant.
func (t *memTx) collectSubtree(id [16]byte) [][16]byte {
	out := [][16]byte{id}
	for i := 0; i < len(out); i++ {
		for _, childID := range t.m.children[out[i]] {
			out = append(out, childID)
		}
	}
	return out
}

func (t *memTx) ExpireVersions(ctx context.Context, retention time.Duration) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := t.m.now().Add(-retention)
	removed := 0
	for _, rec := range t.m.nodes {
		kept := rec.versions[:0]
		for _, v := range rec.versions {
			if v.CreatedAt.Before(cutoff) {
				removed++
				continue
			}
			kept = append(kept, v)
		}
		rec.versions = kept
	}
	return removed, nil
}

var _ LifecycleTx = (*memTx)(nil)

// ExpireRetained moves nodes whose retention has lapsed into the trash.
//
// Mirrors the PostgreSQL statement: eligible when retain_until is in the past and the node is not
// already trashed. Trash rather than delete, so the recovery window still applies.
func (t *memTx) ExpireRetained(ctx context.Context) (int, error) {
	now := t.m.now()
	n := 0
	for id, rec := range t.m.nodes {
		if rec.node.RetainUntil == nil || rec.node.TrashedAt != nil {
			continue
		}
		if rec.node.RetainUntil.After(now) {
			continue
		}
		ts := now
		rec.node.TrashedAt = &ts
		rec.node.ModifiedAt = now
		t.m.nodes[id] = rec

		// Trashing moves the bytes out of the logical quota and into retained usage,
		// whoever caused it. Skipping this left the quota permanently charged for bytes
		// the purge later removed from an uncredited retained counter — the same leak the
		// PostgreSQL backend had (2026-10-24 review, H14). Both backends do it, or the
		// shared suite would prove nothing about the pair.
		if rec.node.Size > 0 {
			owner := rec.node.OwnerID
			if t.m.usage[owner] >= rec.node.Size {
				t.m.usage[owner] -= rec.node.Size
			} else {
				t.m.usage[owner] = 0
			}
			if t.m.trashBytes == nil {
				t.m.trashBytes = map[uuid.UUID]int64{}
			}
			t.m.trashBytes[owner] += rec.node.Size
		}
		n++
	}
	return n, nil
}
