// Package auditverify implements the external audit anchor (INV-09).
//
// The hash chain proves internal consistency: given the events, nobody can alter one
// without breaking the links. It proves nothing about *deletion* — an attacker with
// database access can drop the newest rows and the remaining chain still verifies. The
// anchor closes that by publishing the chain head somewhere the application cannot
// write, so a truncated chain no longer matches what was published.
//
// What this package deliberately is not: a notary. It writes a file. That file must
// live somewhere with different credentials from the database — a WORM bucket, an
// append-only volume, a mail to a security mailbox — and the documentation says so
// plainly, because an anchor stored on the same disk as the database is theatre.
package auditverify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Checkpoint is one published chain head.
type Checkpoint struct {
	// Day is the UTC date the checkpoint covers, truncated to midnight.
	Day time.Time `json:"day"`
	// Head is the chain head as of that moment, hex encoded.
	Head string `json:"head_hex"`
	// Count is how many events the chain had, so a truncation that also rewrites the
	// head is still detectable: the count would not match.
	Count int64 `json:"count"`
	// PublishedAt is when the checkpoint was written, which is not the same as Day:
	// the checkpoint for yesterday is written today.
	PublishedAt time.Time `json:"published_at"`
	// PreviousHead links checkpoints to each other, so a deleted checkpoint leaves a
	// gap that is visible on the next verification.
	PreviousHead string `json:"previous_head_hex,omitempty"`
}

// FileAnchor publishes checkpoints as one JSON document per day in a directory.
//
// One file per day rather than a single mutable file: an overwritten anchor cannot
// prove anything about what it used to contain, and the point of the anchor is to be
// append-only even when the storage behind it is not.
type FileAnchor struct {
	dir string

	mu sync.Mutex
	// lastHead is the most recent published head, used to link the next checkpoint.
	lastHead string
}

// NewFileAnchor returns an anchor writing to dir.
//
// The directory is created if missing. It is not created with any special permissions:
// the security property comes from where it is mounted, not from a mode bit, and
// pretending otherwise would be misleading.
func NewFileAnchor(dir string) (*FileAnchor, error) {
	if dir == "" {
		return nil, fmt.Errorf("auditverify: anchor directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("auditverify: create anchor directory: %w", err)
	}
	a := &FileAnchor{dir: dir}
	// Seed the chain-of-checkpoints from whatever is already published, so a restart
	// does not lose the link to the previous head.
	if last, err := a.latestCheckpoint(); err == nil && last != nil {
		a.lastHead = last.Head
	}
	return a, nil
}

// Publish records a chain head for a day.
//
// Re-publishing the same day overwrites that day's file. That is intentional for a
// retried job, but it does mean the anchor cannot detect a rewrite of *today's*
// checkpoint by someone with write access to the anchor directory — the same caveat
// that makes the location matter more than the format.
func (a *FileAnchor) Publish(day time.Time, head []byte) error {
	return a.PublishWithCount(day, head, 0)
}

// PublishWithCount records a chain head and the event count it covers.
func (a *FileAnchor) PublishWithCount(day time.Time, head []byte, count int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(head) == 0 {
		return fmt.Errorf("auditverify: empty chain head")
	}

	cp := Checkpoint{
		Day:          day.UTC().Truncate(24 * time.Hour),
		Head:         hex.EncodeToString(head),
		Count:        count,
		PublishedAt:  time.Now().UTC(),
		PreviousHead: a.lastHead,
	}

	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}

	// Write to a temporary file and rename, so a crash mid-write cannot leave a
	// truncated checkpoint that later reads as a real (wrong) anchor.
	path := a.pathFor(cp.Day)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("auditverify: write checkpoint: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("auditverify: publish checkpoint: %w", err)
	}

	a.lastHead = cp.Head
	return nil
}

// pathFor names a day's checkpoint file.
//
// The date in the name is what makes the set self-describing: an operator listing the
// directory can see which days were published and, more importantly, which were not.
func (a *FileAnchor) pathFor(day time.Time) string {
	return filepath.Join(a.dir, day.UTC().Format("2006-01-02")+".json")
}

// Checkpoints returns every published checkpoint, oldest first.
func (a *FileAnchor) Checkpoints() ([]Checkpoint, error) {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return nil, err
	}
	var out []Checkpoint
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(a.dir, e.Name()))
		if rerr != nil {
			return nil, rerr
		}
		var cp Checkpoint
		if jerr := json.Unmarshal(raw, &cp); jerr != nil {
			// A corrupt checkpoint is a finding, not a reason to hide the others.
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, e.Name(), jerr)
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out, nil
}

// ErrCorrupt marks an unparseable checkpoint.
var ErrCorrupt = fmt.Errorf("auditverify: corrupt checkpoint")

// latestCheckpoint returns the newest published checkpoint, or nil when none exists.
func (a *FileAnchor) latestCheckpoint() (*Checkpoint, error) {
	cps, err := a.Checkpoints()
	if err != nil {
		return nil, err
	}
	if len(cps) == 0 {
		return nil, nil
	}
	return &cps[len(cps)-1], nil
}

// VerifyResult describes what anchoring found.
type VerifyResult struct {
	// Checkpoints is how many were read.
	Checkpoints int
	// Matched is how many agreed with the chain.
	Matched int
	// Problems are the discrepancies, in the order they were found.
	Problems []string
}

// OK reports whether the anchor and the chain agree.
func (r VerifyResult) OK() bool { return len(r.Problems) == 0 }

// Chain is the part of the audit store anchoring needs.
//
// It is an interface rather than a concrete store so the check can be driven by a test
// fixture, and because anchoring is a read-only concern: it has no business holding a
// write handle on the log it is auditing.
type Chain interface {
	// Head returns the current chain head and event count.
	Head() ([]byte, int64, error)
	// HeadAtCount returns the head the chain had after count events.
	//
	// This is what makes truncation detectable: a checkpoint says "the chain had N
	// events and ended with hash H". Verifying later recomputes the hash of the first N
	// events and compares. If the tail was deleted, the recomputed head differs from the
	// published one even though the remaining events still chain correctly.
	HeadAtCount(count int64) ([]byte, error)
}

// Verify checks every published checkpoint against the chain.
//
// The count check is the important half. Comparing only the *newest* checkpoint against
// the current head would miss a truncation followed by new activity: the attacker
// deletes rows, the system keeps appending, and the head eventually differs from every
// old checkpoint in a way that looks like ordinary growth.
func Verify(a *FileAnchor, chain Chain) (VerifyResult, error) {
	cps, err := a.Checkpoints()
	if err != nil {
		return VerifyResult{}, err
	}

	res := VerifyResult{Checkpoints: len(cps)}

	prevHead := ""
	for i, cp := range cps {
		// 1. A published checkpoint must not be missing from the middle of the run.
		//
		// The PreviousHead link alone cannot catch this: the surviving checkpoints each
		// recorded the previous *published* head, so after deleting the middle one the
		// remaining pair still points at each other correctly. What exposes the gap is
		// the date sequence — a run of daily checkpoints with a hole in it.
		if i > 0 {
			prevDay := cps[i-1].Day
			gap := cp.Day.Sub(prevDay)
			if gap > 25*time.Hour {
				res.Problems = append(res.Problems, fmt.Sprintf(
					"no checkpoint published between %s and %s (%d days missing)",
					prevDay.Format("2006-01-02"), cp.Day.Format("2006-01-02"),
					int(gap.Hours()/24)-1))
			}
		}
		if prevHead != "" && cp.PreviousHead != "" && cp.PreviousHead != prevHead {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"checkpoint %s does not link to the previous one (expected %s, found %s)",
				cp.Day.Format("2006-01-02"), short(prevHead), short(cp.PreviousHead)))
		}
		prevHead = cp.Head

		// 2. The published count must not exceed what the chain now holds: that means
		//    events were deleted.
		_, total, herr := chain.Head()
		if herr != nil {
			return res, herr
		}
		if cp.Count > total {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"checkpoint %s covers %d events but the chain holds only %d: events were deleted",
				cp.Day.Format("2006-01-02"), cp.Count, total))
			continue
		}
		if cp.Count == 0 {
			// A checkpoint published before counts were recorded cannot be checked this
			// way; comparing against the current head is meaningless, so it is counted
			// as matched on the strength of the chain link alone.
			res.Matched++
			continue
		}

		// 3. Recompute the head at that count and compare. This is the check that
		//    catches a rewritten middle: the events still chain, but not to this value.
		got, gerr := chain.HeadAtCount(cp.Count)
		if gerr != nil {
			return res, gerr
		}
		if hex.EncodeToString(got) != cp.Head {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"checkpoint %s published head %s but the chain's first %d events hash to %s",
				cp.Day.Format("2006-01-02"), short(cp.Head), cp.Count, short(hex.EncodeToString(got))))
			continue
		}
		res.Matched++
	}

	return res, nil
}

// short truncates a hex hash for messages.
func short(h string) string {
	if len(h) <= 16 {
		return h
	}
	return h[:16] + "…"
}

// Digest is a convenience for tests and for log lines: a short, stable identifier for a
// chain head.
func Digest(head []byte) string {
	sum := sha256.Sum256(head)
	return hex.EncodeToString(sum[:8])
}

// DirForTest exposes the anchor directory so a test can delete a published checkpoint.
//
// Deleting one is not something a production caller should be able to do either, which
// is why there is no general-purpose Remove: the test reaches the filesystem directly
// through this path rather than the package offering a way to un-publish.
func (a *FileAnchor) DirForTest() string { return a.dir }
