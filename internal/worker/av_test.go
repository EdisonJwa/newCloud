package worker

// Antivirus job tests (M5). The scanner is the package's own interface, so these speak
// verdicts without a daemon; the protocol the daemon speaks is covered by internal/av
// against a real TCP listener. What only this package can prove is the CONSEQUENCE of a
// verdict: quarantine, notify and audit land in one transaction, and the clean path touches
// nothing.

import (
	"context"
	"crypto/sha256"
	"io"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/av"
	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// verdictScanner answers every scan with a fixed verdict.
type verdictScanner struct {
	verdict av.Verdict
	sig     string
	err     error
	scanned int
}

func (s *verdictScanner) Scan(ctx context.Context, r io.Reader) (av.Verdict, string, error) {
	s.scanned++
	_, _ = io.Copy(io.Discard, r)
	return s.verdict, s.sig, s.err
}

// blobReaderFor satisfies the scrub's BlobReader with in-memory bytes.
type blobReaderFor struct{ data string }

func (b blobReaderFor) Open(ctx context.Context, h blob.Hash) (io.ReadSeekCloser, error) {
	return nopCloser{strings.NewReader(b.data)}, nil
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

// infectedFixture creates a user, a root, a file node whose blob is registered, and returns
// the node. The blob id comes from the store's own BlobIDFor, exactly as the publish path
// obtains it.
func infectedFixture(t *testing.T, m *store.Memory) (store.User, store.Node) {
	t.Helper()
	u := m.AddUser(store.User{Username: "av-owner", Email: "av@example.test", Role: "user"})
	content := "X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*"
	sum := sha256.Sum256([]byte(content))
	blobID := m.BlobIDFor(context.Background(), sum, int64(len(content)))

	var node store.Node
	err := m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		root, rerr := tx.EnsureRoot(ctx, u.ID)
		if rerr != nil {
			return rerr
		}
		n, _, werr := tx.WriteFile(ctx, store.WriteFile{
			NewFile: store.NewFile{
				OwnerID: u.ID, ParentID: root.ID, Name: "eicar.txt",
				Size: int64(len(content)), Mime: "text/plain",
				ETag: "probe", BlobID: blobID, BlobHash: sum,
			},
		})
		node = n
		return werr
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return u, node
}

func TestAVScanQuarantinesNotifiesAndAuditsInOnePass(t *testing.T) {
	w, m := testWorker(t)
	scanner := &verdictScanner{verdict: av.Infected, sig: "Win.Test.EICAR.HSTR-1"}
	w.WithAntivirus(scanner, 1<<20).WithBlobScrub(blobReaderFor{data: "anything"}, 0)
	owner, node := infectedFixture(t, m)

	enqueue(t, m, store.Job{Kind: "av.scan", Payload: map[string]any{
		"node_id": node.ID.String()}})
	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Quarantined: the node is gone from the live tree (it sits in the owner's trash).
	// Notified: the owner's inbox holds exactly the av.quarantined row.
	var live bool
	var kinds []string
	err := m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		if _, gerr := tx.Node(ctx, node.ID); gerr == nil {
			live = true
		}
		items, _, _, lerr := tx.Notifications(ctx, owner.ID, "", 10)
		for _, n := range items {
			kinds = append(kinds, n.Kind)
		}
		return lerr
	})
	if err != nil {
		t.Fatal(err)
	}
	if live {
		t.Fatal("an infected file is still live after the scan")
	}
	if len(kinds) != 1 || kinds[0] != store.NotifyAVQuarantined {
		t.Fatalf("owner notifications: %v, want one av.quarantined", kinds)
	}
}

func TestAVScanCleanLeavesEverythingAlone(t *testing.T) {
	w, m := testWorker(t)
	scanner := &verdictScanner{verdict: av.Clean}
	w.WithAntivirus(scanner, 1<<20).WithBlobScrub(blobReaderFor{data: "plain"}, 0)
	owner, node := infectedFixture(t, m)

	enqueue(t, m, store.Job{Kind: "av.scan", Payload: map[string]any{
		"node_id": node.ID.String()}})
	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}

	err := m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		if _, gerr := tx.Node(ctx, node.ID); gerr != nil {
			t.Fatalf("a clean scan removed the file: %v", gerr)
		}
		n, cerr := tx.UnreadNotificationCount(ctx, owner.ID)
		if cerr != nil {
			return cerr
		}
		if n != 0 {
			t.Fatalf("a clean scan notified the owner %d times", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanner.scanned != 1 {
		t.Fatalf("scanned %d times, want 1", scanner.scanned)
	}
}

func TestAVScanRetriesWhenTheDaemonIsDown(t *testing.T) {
	w, m := testWorker(t)
	w.WithAntivirus(&verdictScanner{err: av.ErrScannerUnavailable}, 1<<20).
		WithBlobScrub(blobReaderFor{data: "x"}, 0)
	_, node := infectedFixture(t, m)

	enqueue(t, m, store.Job{Kind: "av.scan", Payload: map[string]any{
		"node_id": node.ID.String()}})
	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, _, failed, err := jobCounts(t, w)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 || pending != 1 {
		t.Fatalf("a daemon outage must be retryable: pending=%d failed=%d", pending, failed)
	}
}

func TestAVScanWithoutAScannerCompletes(t *testing.T) {
	// The deployment that turned scanning off after uploads queued jobs: the job must
	// complete with a warning, not dead-letter — the file is not infected, merely unscanned.
	w, m := testWorker(t)
	_, node := infectedFixture(t, m)

	enqueue(t, m, store.Job{Kind: "av.scan", Payload: map[string]any{
		"node_id": node.ID.String()}})
	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, failed, err := jobCounts(t, w); err != nil || failed != 0 {
		t.Fatalf("failed=%d err=%v: an unscannable job must complete, not dead-letter", failed, err)
	}
}

func TestAVScanRefusesAMalformedPayload(t *testing.T) {
	w, m := testWorker(t)
	w.WithAntivirus(&verdictScanner{verdict: av.Clean}, 0)

	enqueue(t, m, store.Job{Kind: "av.scan", Payload: map[string]any{
		"node_id": "not-a-uuid"}})
	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, failed, err := jobCounts(t, w); err != nil || failed != 1 {
		t.Fatalf("a malformed payload must dead-letter exactly once: failed=%d err=%v", failed, err)
	}
}
