package storetest

// Login-flow conformance (ADR-0015).
//
// The Nextcloud login flow is the one place where an anonymous caller can cause a
// credential to be issued. The properties that keep that safe are store properties, not
// handler properties: a flow must be approvable by exactly one account, a poll must
// consume it exactly once, and an expired flow must be indistinguishable from one that
// never existed. The in-memory backend is what most tests run against, so a divergence
// there would hide a real defect — which is exactly what this suite exists to prevent.

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// LoginFlowCases returns the login-flow block of the shared suite.
func LoginFlowCases(factory Factory) func(t *testing.T) {
	return func(t *testing.T) {
		// uniqueHash gives each subtest its own token digests. PostgreSQL's table is
		// shared across subtests, and a literal hash would collide on the unique index.
		uniqueHash := func(label string) [32]byte {
			return sha256.Sum256([]byte(label + "/" + uuid.NewString()))
		}

		createFlow := func(t *testing.T, s store.Store, expires time.Time) (store.LoginFlow, [32]byte, [32]byte) {
			t.Helper()
			now := time.Now()
			flow := store.LoginFlow{
				ID: uuid.New(), ClientName: "Nextcloud Desktop Client",
				CreatedAt: now, ExpiresAt: expires,
			}
			pollHash, loginHash := uniqueHash("poll"), uniqueHash("login")
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.CreateLoginFlow(ctx, flow, pollHash, loginHash)
			}); err != nil {
				t.Fatalf("create login flow: %v", err)
			}
			return flow, pollHash, loginHash
		}

		t.Run("AStartedFlowIsPendingNotApproved", func(t *testing.T) {
			s, _ := factory(t)

			f, _, loginHash := createFlow(t, s, time.Now().Add(time.Hour))
			var got store.LoginFlow
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				var err error
				got, err = tx.LoginFlowByLoginHash(ctx, loginHash)
				return err
			}); err != nil {
				t.Fatalf("resolve by login token: %v", err)
			}
			if got.ID != f.ID {
				t.Fatalf("resolved flow %s, want %s", got.ID, f.ID)
			}
			if got.Approved() {
				t.Fatal("a freshly created flow must not count as approved; approving is what " +
					"authorises the credential, and skipping it would let the client issue one " +
					"without the user ever seeing the consent page")
			}

			// Polling before approval must find nothing. Returning a row here would mint
			// an App Password for a client the user never approved.
			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, cerr := tx.ConsumeLoginFlow(ctx, sha256.Sum256([]byte("wrong")), time.Now())
				return cerr
			})
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("consume with an unknown token returned %v, want ErrNotFound", err)
			}
		})

		t.Run("ApprovalIsConditionalOnOneAccount", func(t *testing.T) {
			s, owner := factory(t)
			other := addUser(t, s, "nf-other-"+uuid.NewString()[:8])

			_, _, loginHash := createFlow(t, s, time.Now().Add(time.Hour))
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.ApproveLoginFlow(ctx, loginHash, owner, time.Now())
				return err
			}); err != nil {
				t.Fatalf("first approval: %v", err)
			}

			// A second account following the same browser link must not take the flow
			// over: the client would then receive a credential for the wrong user.
			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, aerr := tx.ApproveLoginFlow(ctx, loginHash, other.ID, time.Now())
				return aerr
			})
			if !errors.Is(err, store.ErrLoginFlowConflict) {
				t.Fatalf("second account approving returned %v, want ErrLoginFlowConflict", err)
			}

			// The first account re-submitting its own approval is a double form submit,
			// not a conflict.
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, aerr := tx.ApproveLoginFlow(ctx, loginHash, owner, time.Now())
				return aerr
			}); err != nil {
				t.Fatalf("re-approval by the same account: %v", err)
			}
		})

		t.Run("ConsumingAnApprovedFlowIsSingleUse", func(t *testing.T) {
			s, owner := factory(t)

			_, pollHash, loginHash := createFlow(t, s, time.Now().Add(time.Hour))
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.ApproveLoginFlow(ctx, loginHash, owner, time.Now())
				return err
			}); err != nil {
				t.Fatalf("approve: %v", err)
			}

			var consumed store.LoginFlow
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				var err error
				consumed, err = tx.ConsumeLoginFlow(ctx, pollHash, time.Now())
				return err
			}); err != nil {
				t.Fatalf("first consume: %v", err)
			}
			if consumed.UserID == nil || *consumed.UserID != owner {
				t.Fatalf("consumed flow names user %v, want %s", consumed.UserID, owner)
			}

			// The second poll is what a duplicated poll request looks like. It must not
			// receive a second credential.
			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, cerr := tx.ConsumeLoginFlow(ctx, pollHash, time.Now())
				return cerr
			})
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second consume returned %v, want ErrNotFound", err)
			}

			// The browser token went with the row: an approval after the poll cannot
			// resurrect it.
			err = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, aerr := tx.ApproveLoginFlow(ctx, loginHash, owner, time.Now())
				return aerr
			})
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("approving a consumed flow returned %v, want ErrNotFound", err)
			}
		})

		t.Run("ExpiredFlowsAreIndistinguishableFromUnknownOnes", func(t *testing.T) {
			s, owner := factory(t)

			_, pollHash, loginHash := createFlow(t, s, time.Now().Add(-time.Minute))
			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, aerr := tx.ApproveLoginFlow(ctx, loginHash, owner, time.Now())
				return aerr
			})
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("approving an expired flow returned %v, want ErrNotFound", err)
			}
			err = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, cerr := tx.ConsumeLoginFlow(ctx, pollHash, time.Now())
				return cerr
			})
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("consuming an expired flow returned %v, want ErrNotFound", err)
			}
		})

		t.Run("ExpiredRowsAreSweptWithoutTouchingLiveOnes", func(t *testing.T) {
			s, owner := factory(t)

			createFlow(t, s, time.Now().Add(-time.Hour))
			_, pollHash, loginHash := createFlow(t, s, time.Now().Add(time.Hour))

			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, derr := tx.DeleteExpiredLoginFlows(ctx, time.Now())
				if derr != nil {
					return derr
				}
				// The PostgreSQL table is shared with earlier subtests, so the count is a
				// lower bound rather than an exact figure.
				if n < 1 {
					t.Fatalf("swept %d rows, want at least the one that expired", n)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				if _, aerr := tx.ApproveLoginFlow(ctx, loginHash, owner, time.Now()); aerr != nil {
					return aerr
				}
				_, cerr := tx.ConsumeLoginFlow(ctx, pollHash, time.Now())
				return cerr
			}); err != nil {
				t.Fatalf("the live flow did not survive the sweep: %v", err)
			}
		})
	}
}
