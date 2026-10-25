package worker

// The antivirus scan job (M5).
//
// Design in one paragraph: uploads enqueue an av.scan job in the SAME transaction as the
// publish (the enqueueSearchIndex precedent), and the worker scans asynchronously — a request
// must not wait on a malware daemon, exactly as it must not wait on a search engine or a mail
// relay. The cost is a window in which an infected file is briefly readable; the payment is
// that a down daemon degrades to "scans pending" instead of "uploads refused". On a detection
// the job quarantines: trash the node, notify the owner, audit the action — one transaction,
// never a silent deletion. Nextcloud's files_antivirus made the same asynchronous choice.

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/av"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/google/uuid"
)

// WithAntivirus wires the scanner. Nil means the deployment runs without one: av.scan jobs
// complete with a warning rather than dead-letter, because a message nobody can act on does
// not become actionable by retrying.
func (w *Worker) WithAntivirus(s av.Scanner, maxScanBytes int64) *Worker {
	w.scanner = s
	w.avMaxBytes = maxScanBytes
	return w
}

// scanNode runs one av.scan payload: read the node, stream its bytes to the daemon, act on
// the verdict.
func (w *Worker) scanNode(ctx context.Context, payload map[string]any) error {
	idRaw, ok := payload["node_id"].(string)
	if !ok || idRaw == "" {
		return Permanent(fmt.Errorf("av.scan: missing node_id"))
	}
	nodeID, err := uuid.Parse(idRaw)
	if err != nil {
		return Permanent(fmt.Errorf("av.scan: bad node_id %q: %w", idRaw, err))
	}

	if w.scanner == nil {
		w.log.Warn("av.scan completed without a scan: no scanner configured", "node", nodeID)
		return nil
	}

	// The node may be gone (trashed by its owner before the job ran): nothing to scan, and
	// saying so is the correct completion, not an error.
	var node store.Node
	var blobID *uuid.UUID
	if err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, gerr := tx.Node(ctx, nodeID)
		if gerr == store.ErrNotFound {
			return nil
		}
		if gerr != nil {
			return gerr
		}
		node, blobID = n, n.BlobID
		return nil
	}); err != nil {
		return err
	}
	if node.ID == uuid.Nil || blobID == nil || node.TrashedAt != nil {
		return nil
	}

	// The bytes come from the same BlobReader the integrity scrub uses — content-addressed
	// reads, which is the blob layout. The tree layout keeps no hash-addressable copy, so its
	// jobs complete with a warning: the same honest split version-restore already makes.
	if w.blobs == nil {
		w.log.Warn("av.scan completed without a scan: no content reader for raw bytes",
			"node", nodeID)
		return nil
	}
	var hashBytes []byte
	if err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		h, herr := tx.BlobHash(ctx, *blobID)
		hashBytes = h
		return herr
	}); err != nil {
		return err
	}
	if len(hashBytes) != 32 {
		w.log.Warn("av.scan completed without a scan: no content address for the node's bytes",
			"node", nodeID)
		return nil
	}
	var hash [32]byte
	copy(hash[:], hashBytes)

	rc, err := w.blobs.Open(ctx, hash)
	if err != nil {
		return fmt.Errorf("av.scan: open content: %w", err)
	}
	defer rc.Close()

	limit := w.avMaxBytes
	if limit <= 0 {
		limit = 100 << 20 // a daemon default-sized ceiling, not an unbounded one
	}
	verdict, sig, serr := w.scanner.Scan(ctx, io.LimitReader(rc, limit))
	if serr != nil {
		if errors.Is(serr, av.ErrScannerUnavailable) {
			// Retryable: the daemon being restarted is the common failure, and the queue's
			// backoff is the correct response to it.
			return fmt.Errorf("av.scan: %w", serr)
		}
		return Permanent(fmt.Errorf("av.scan: %w", serr))
	}
	if verdict == av.Clean {
		return nil
	}

	// Quarantine: trash + notify + audit in one transaction, so a detection can never be half
	// applied — a file trashed without the notification is a disappearance with no explanation,
	// and a notification without the trash is an alarm about a file still readable.
	return w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if terr := tx.Trash(ctx, node.ID); terr != nil && terr != store.ErrNotFound {
			return fmt.Errorf("av.scan: quarantine trash: %w", terr)
		}
		if nerr := tx.Notify(ctx, store.NewNotification{
			UserID: node.OwnerID, Kind: store.NotifyAVQuarantined,
			DedupeKey: "av:" + node.ID.String(),
			Meta:      map[string]any{"name": node.Name, "signature": sig},
		}); nerr != nil {
			return nerr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "system", Action: string(audit.ActionAVQuarantine),
			TargetKind: "node", TargetID: node.ID.String(), Result: "ok",
			Metadata: map[string]any{
				"node": node.Name, "owner": node.OwnerID.String(), "signature": sig,
			},
		})
	})
}
