package auditverify

// Store adapter: the audit chain as the anchor verifier sees it.
//
// It lives here rather than in internal/store so the store keeps no dependency on the
// anchoring policy, and so the store's audit surface stays a plain read API. The
// adapter is deliberately read-only: a verifier that can also write is one compromised
// code path away from repairing a chain that should be reported broken.

import (
	"context"

	"github.com/EdisonJwa/newCloud/internal/audit"
)

// RowSource is the store's audit read surface.
type RowSource interface {
	// AuditRows returns up to limit events after afterID, oldest first.
	AuditRows(ctx context.Context, afterID int64, limit int) ([]audit.Event, error)
	// AuditHead returns the stored chain head.
	AuditHead(ctx context.Context) ([]byte, error)
}

// StoreChain adapts RowSource to Chain.
//
// HeadAtCount is the operation the whole design rests on: it recomputes the chain as it
// stood after N events and returns its head. Comparing that against a published
// checkpoint is what turns "the chain is internally consistent" into "the chain has not
// been shortened".
type StoreChain struct {
	src   RowSource
	batch int
	// cache keeps computed heads per count, because verifying a year of daily
	// checkpoints would otherwise re-walk the chain once per checkpoint.
	cache map[int64][]byte
}

// NewStoreChain returns a Chain over src.
func NewStoreChain(src RowSource) *StoreChain {
	return &StoreChain{src: src, batch: 512, cache: map[int64][]byte{}}
}

// Head returns the current head and total event count.
func (c *StoreChain) Head() ([]byte, int64, error) {
	// The count has to come from walking the rows: the store keeps a head but not a
	// count, and a count that could disagree with the rows would be the one field that
	// hides a truncation.
	ctx := context.Background()
	var (
		after int64
		count int64
		head  = audit.GenesisHash
	)
	for {
		rows, err := c.src.AuditRows(ctx, after, c.batch)
		if err != nil {
			return nil, 0, err
		}
		if len(rows) == 0 {
			break
		}
		for _, e := range rows {
			count++
			head = e.Hash
			after = e.ID
		}
		if len(rows) < c.batch {
			break
		}
	}
	return head, count, nil
}

// HeadAtCount returns the chain head as it stood after the first count events.
func (c *StoreChain) HeadAtCount(count int64) ([]byte, error) {
	if count <= 0 {
		return audit.GenesisHash, nil
	}
	if h, ok := c.cache[count]; ok {
		return h, nil
	}

	ctx := context.Background()
	var (
		after int64
		seen  int64
		head  = audit.GenesisHash
	)
	for seen < count {
		limit := c.batch
		if remaining := count - seen; remaining < int64(limit) {
			limit = int(remaining)
		}
		rows, err := c.src.AuditRows(ctx, after, limit)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			// The chain is shorter than the checkpoint claims. Returning the head
			// reached so far makes the comparison fail, which is the correct verdict:
			// events were deleted.
			break
		}
		for _, e := range rows {
			head = e.Hash
			after = e.ID
			seen++
			if seen == count {
				break
			}
		}
	}

	c.cache[count] = head
	return head, nil
}

var _ Chain = (*StoreChain)(nil)
