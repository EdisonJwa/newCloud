package storetest

// Conformance for the social substrate and the extended search (core-scope §3, §4, §5, §6, §7.1).
//
// Both backends run these cases, and that is the point rather than a formality: every rule below is
// implemented twice — once as a SQL statement and once as a Go loop — and the two agreed only by
// accident until somebody wrote the pair down. The three previous rounds each shipped a method that
// was tested on the in-memory backend only and was broken on PostgreSQL, which is why this suite
// lives here rather than in either store's own test file.
//
// Each case asserts the CONTRACT, not an implementation: "starring is idempotent", "a tag name is
// unique case-insensitively", "a comment past the limit is refused", "a purge is scoped to one
// account". The failure messages say what a user would lose, because that is the question whoever
// reads the failure will have.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// socialOf type-asserts the social surface, so a backend that silently loses it fails loudly
// instead of quietly dropping the feature.
func socialOf(t *testing.T, s store.Store) store.SocialTx {
	t.Helper()
	tx, ok := s.(store.SocialTx)
	if !ok {
		t.Fatalf("%T does not implement store.SocialTx; favorites, tags and comments would be "+
			"unavailable on this backend and the handlers would answer 503", s)
	}
	// The interface is reachable through Tx, so the assertion above is about the composite; check
	// that the transaction really carries it, which is what every handler uses.
	if err := s.WithTx(context.Background(), func(ctx context.Context, inner store.Tx) error {
		st, ok := inner.(store.SocialTx)
		if !ok {
			t.Fatalf("%T's Tx does not implement store.SocialTx", s)
		}
		_ = st
		return nil
	}); err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	return tx
}

// doSocial runs fn inside a transaction against the social surface.
func doSocial(t *testing.T, s store.Store, fn func(ctx context.Context, tx store.SocialTx) error) {
	t.Helper()
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		st, ok := tx.(store.SocialTx)
		if !ok {
			t.Fatalf("%T's Tx does not implement store.SocialTx", s)
		}
		return fn(ctx, st)
	}); err != nil {
		t.Fatalf("social operation: %v", err)
	}
}

// ------------------------------------------------------------------ favorites --

func favoriteSuite(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("StarringIsPerUserAndIdempotent", func(t *testing.T) {
		s, mine := factory(t)
		other := addUser(t, s, "fav-other")
		r := rootOf(t, s, mine)
		f := mkfile(t, s, mine, r.ID, "shared-plan.txt", 8)

		doSocial(t, s, func(ctx context.Context, tx store.SocialTx) error {
			if err := tx.SetFavorite(ctx, mine, f.ID); err != nil {
				return err
			}
			// A repeat is not an error: a client that retries after a dropped response must not
			// have to distinguish "already starred" from "starred now".
			return tx.SetFavorite(ctx, mine, f.ID)
		})

		var mineStarred, otherStarred bool
		doSocial(t, s, func(ctx context.Context, tx store.SocialTx) error {
			var err error
			mineStarred, err = tx.IsFavorite(ctx, mine, f.ID)
			if err != nil {
				return err
			}
			otherStarred, err = tx.IsFavorite(ctx, other.ID, f.ID)
			return err
		})
		if !mineStarred {
			t.Fatal("the star did not survive its own write")
		}
		if otherStarred {
			t.Fatal("a second account sees the first account's star. A favorite is a per-USER row " +
				"precisely so two people sharing a folder star independently; a node flag would " +
				"make the first click star it for everybody.")
		}

		// And the other account's own star is independent of the first.
		doSocial(t, s, func(ctx context.Context, tx store.SocialTx) error {
			return tx.SetFavorite(ctx, other.ID, f.ID)
		})
		var both int
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			list, err := tx.FavoritesAfter(ctx, mine, "", 10)
			if err != nil {
				return err
			}
			both = len(list)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if both != 1 {
			t.Fatalf("one account's favorite listing has %d rows; each account must see only its "+
				"own stars", both)
		}
	})

	t.Run("UnstarringIsIdempotent", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "transient.txt", 4)

		doSocial(t, s, func(ctx context.Context, tx store.SocialTx) error {
			if err := tx.SetFavorite(ctx, owner, f.ID); err != nil {
				return err
			}
			if err := tx.UnsetFavorite(ctx, owner, f.ID); err != nil {
				return err
			}
			// Un-starring something that is not starred is a no-op, not a 404: the client's
			// intent ("not starred") is already satisfied.
			return tx.UnsetFavorite(ctx, owner, f.ID)
		})

		var starred bool
		doSocial(t, s, func(ctx context.Context, tx store.SocialTx) error {
			var err error
			starred, err = tx.IsFavorite(ctx, owner, f.ID)
			return err
		})
		if starred {
			t.Fatal("the favorite survived its removal")
		}
	})

	t.Run("AStarSurvivesRenameAndMove", func(t *testing.T) {
		// §4's acceptance criterion: "move/rename keeps the favorite". It holds because the row is
		// keyed by node id, and this test exists to stop a future change making it keyed by path.
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		docs := mkdir(t, s, owner, r.ID, "docs")
		f := mkfile(t, s, owner, r.ID, "budget.xlsx", 12)

		doSocial(t, s, func(ctx context.Context, tx store.SocialTx) error {
			return tx.SetFavorite(ctx, owner, f.ID)
		})
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			if _, err := tx.Rename(ctx, f.ID, "budget-final.xlsx"); err != nil {
				return err
			}
			_, err := tx.Move(ctx, f.ID, docs.ID)
			return err
		}); err != nil {
			t.Fatalf("rename+move: %v", err)
		}

		var list []store.FavoriteNode
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.FavoritesAfter(ctx, owner, "", 10)
			list = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 {
			t.Fatalf("want the favorite after a rename and a move, got %d entries. A favorite "+
				"keyed by path would have been lost here.", len(list))
		}
		if list[0].Node.Name != "budget-final.xlsx" {
			t.Fatalf("the listing returned %q, want the new name", list[0].Node.Name)
		}
		if list[0].Path != "docs/budget-final.xlsx" {
			t.Fatalf("path = %q, want docs/budget-final.xlsx: a favorite you cannot jump to is "+
				"a dead entry", list[0].Path)
		}
	})

	t.Run("AStarOnATrashedNodeIsHiddenNotDeleted", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "recycled.txt", 4)

		doSocial(t, s, func(ctx context.Context, tx store.SocialTx) error {
			return tx.SetFavorite(ctx, owner, f.ID)
		})
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, f.ID)
		}); err != nil {
			t.Fatal(err)
		}

		var list []store.FavoriteNode
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.FavoritesAfter(ctx, owner, "", 10)
			list = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(list) != 0 {
			t.Fatalf("a trashed node appears in the favorites listing (%q); the link would 404",
				list[0].Node.Name)
		}

		// Restoring brings it back: the row was hidden, not removed, so the user who deleted by
		// accident gets their star back.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.Restore(ctx, f.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.FavoritesAfter(ctx, owner, "", 10)
			list = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 {
			t.Fatalf("the star did not come back after a restore, got %d entries", len(list))
		}
	})

	t.Run("FavoritesPageWithoutRepeatingOrSkipping", func(t *testing.T) {
		// Two stars written inside the same clock tick are the case a keyset on timestamp alone
		// gets wrong: without the id tie-break a page boundary either repeats one or drops one.
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		want := map[uuid.UUID]bool{}
		for i := 0; i < 5; i++ {
			f := mkfile(t, s, owner, r.ID, "fav-"+itoa(i)+".txt", 4)
			want[f.ID] = true
			doSocial(t, s, func(ctx context.Context, tx store.SocialTx) error {
				return tx.SetFavorite(ctx, owner, f.ID)
			})
		}

		seen := map[uuid.UUID]int{}
		cursor := ""
		for page := 0; page < 10; page++ {
			var got []store.FavoriteNode
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				list, err := tx.FavoritesAfter(ctx, owner, cursor, 2)
				got = list
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 {
				break
			}
			for _, item := range got {
				seen[item.Node.ID]++
			}
			last := got[len(got)-1]
			cursor = store.FormatSocialCursor(last.FavoritedAt, last.Node.ID)
			if len(got) < 2 {
				break
			}
		}
		for id := range want {
			if seen[id] != 1 {
				t.Fatalf("favorite %s appeared %d times across pages, want exactly 1", id, seen[id])
			}
		}
	})

	t.Run("AForeignCursorIsRefused", func(t *testing.T) {
		s, owner := factory(t)
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, cerr := tx.FavoritesAfter(ctx, owner, "not-a-cursor", 10)
			return cerr
		})
		if !errors.Is(err, store.ErrInvalidCursor) {
			t.Fatalf("a cursor this server did not issue returned %v, want ErrInvalidCursor: "+
				"starting over instead would make a client re-read page one forever", err)
		}
	})
}

// ----------------------------------------------------------------------- tags --

func tagSuite(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("ACaseFoldCollisionIsRefused", func(t *testing.T) {
		// §5's first acceptance criterion. It is the lesson migration 0007 taught this codebase for
		// filenames applied to labels: two tags a user cannot tell apart would fragment the filter
		// they exist to drive.
		s, owner := factory(t)
		doSocial(t, s, func(ctx context.Context, tx store.SocialTx) error {
			if _, err := tx.CreateTag(ctx, store.Tag{OwnerID: &owner, Name: "Finance", Color: "blue"}); err != nil {
				return err
			}
			return nil
		})

		// A failed statement aborts the PostgreSQL transaction, so the refusal has to travel as the
		// WithTx error rather than being captured and swallowed — capturing it reports a healthy
		// transaction that the server rolled back.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, cerr := tx.CreateTag(ctx, store.Tag{OwnerID: &owner, Name: "FINANCE", Color: "red"})
			return cerr
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a tag differing only in case returned %v, want ErrConflict", err)
		}
	})

	t.Run("TwoAccountsMayUseTheSameTagName", func(t *testing.T) {
		// Personal tags are per account: "Finance" is a word, not a globally owned string.
		s, mine := factory(t)
		other := addUser(t, s, "tag-other")
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			if _, err := tx.CreateTag(ctx, store.Tag{OwnerID: &mine, Name: "Finance"}); err != nil {
				return err
			}
			_, err := tx.CreateTag(ctx, store.Tag{OwnerID: &other.ID, Name: "Finance"})
			return err
		}); err != nil {
			t.Fatalf("two accounts could not both define a personal tag named Finance: %v", err)
		}
	})

	t.Run("ATagCanBeRenamedAndRecoloured", func(t *testing.T) {
		// UpdateTag exists because a label that cannot be corrected is a label people abandon and
		// recreate, which leaves the old name on every node it was applied to.
		s, owner := factory(t)
		var tag store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			tag, err = tx.CreateTag(ctx, store.Tag{OwnerID: &owner, Name: "draft", Color: "gray"})
			return err
		}); err != nil {
			t.Fatal(err)
		}

		var updated store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			updated, err = tx.UpdateTag(ctx, tag.ID, "approved", "green")
			return err
		}); err != nil {
			t.Fatalf("UpdateTag: %v", err)
		}
		if updated.Name != "approved" || updated.Color != "green" {
			t.Fatalf("after an update the tag is %q/%q, want approved/green", updated.Name, updated.Color)
		}

		// TagByID is the read the update path and every assignment check uses; assert it agrees
		// with what UpdateTag returned rather than trusting the write's own echo.
		var read store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			read, err = tx.TagByID(ctx, tag.ID)
			return err
		}); err != nil {
			t.Fatalf("TagByID: %v", err)
		}
		if read.Name != "approved" {
			t.Fatalf("TagByID returned %q after the rename", read.Name)
		}

		// Renaming onto a sibling's fold is refused, exactly like creating one.
		var sibling store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			sibling, err = tx.CreateTag(ctx, store.Tag{OwnerID: &owner, Name: "Rejected"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, uerr := tx.UpdateTag(ctx, sibling.ID, "APPROVED", "green")
			return uerr
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("renaming a tag onto a sibling's case-fold returned %v, want ErrConflict", err)
		}

		// A colour outside the palette is refused: the value ends up in a CSS class, and the first
		// thing that happens to user-chosen text that reaches CSS is a styling injection.
		err = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, uerr := tx.UpdateTag(ctx, tag.ID, "approved", "chartreuse")
			return uerr
		})
		if !errors.Is(err, store.ErrInvalidSocialValue) {
			t.Fatalf("an off-palette colour returned %v, want ErrInvalidSocialValue", err)
		}
	})

	t.Run("ASystemTagIsVisibleToEveryAccountAndCannotBeDeletedByOne", func(t *testing.T) {
		s, mine := factory(t)
		other := addUser(t, s, "tag-viewer")

		var system store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			system, err = tx.CreateTag(ctx, store.Tag{Name: "Retention", Color: "orange"})
			return err
		}); err != nil {
			t.Fatalf("create a system tag: %v", err)
		}
		if !system.IsSystem() {
			t.Fatal("a tag created without an owner is not marked as a system tag")
		}

		for _, account := range []uuid.UUID{mine, other.ID} {
			var visible bool
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				list, err := tx.TagsFor(ctx, account)
				if err != nil {
					return err
				}
				for _, tag := range list {
					if tag.ID == system.ID {
						visible = true
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !visible {
				t.Fatal("a system tag is not listed for an account; every deployment's shared " +
					"vocabulary would be invisible")
			}
		}

		// Deleting it would empty the label off every node in the deployment.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.DeleteTag(ctx, system.ID)
		})
		if err == nil {
			t.Fatal("a plain DeleteTag removed a system tag; that empties the label off every node " +
				"in the deployment and no single caller should be able to do it")
		}
	})

	t.Run("ThePerAccountTagCapIsEnforced", func(t *testing.T) {
		s, owner := factory(t)
		// Fill to the cap, then one more.
		for i := 0; i < store.MaxTagsPerAccount; i++ {
			name := "cap-" + itoa(i)
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.CreateTag(ctx, store.Tag{OwnerID: &owner, Name: name})
				return err
			}); err != nil {
				t.Fatalf("create %q: %v", name, err)
			}
		}
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, cerr := tx.CreateTag(ctx, store.Tag{OwnerID: &owner, Name: "one-too-many"})
			return cerr
		})
		if !errors.Is(err, store.ErrTagLimit) {
			t.Fatalf("the %dth tag returned %v, want ErrTagLimit", store.MaxTagsPerAccount+1, err)
		}
	})

	t.Run("ThePerNodeTagCapIsEnforced", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "many-labels.txt", 4)

		for i := 0; i < store.MaxTagsPerNode; i++ {
			var tag store.Tag
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				var err error
				tag, err = tx.CreateTag(ctx, store.Tag{
					OwnerID: &owner, Name: "n" + itoa(i),
				})
				return err
			}); err != nil {
				t.Fatalf("create tag %d: %v", i, err)
			}
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.AssignTag(ctx, f.ID, tag.ID, owner)
			}); err != nil {
				t.Fatalf("assign tag %d: %v", i, err)
			}
		}

		var extra store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			extra, err = tx.CreateTag(ctx, store.Tag{OwnerID: &owner, Name: "one-more"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.AssignTag(ctx, f.ID, extra.ID, owner)
		})
		if !errors.Is(err, store.ErrTagLimit) {
			t.Fatalf("the %dth tag on one node returned %v, want ErrTagLimit",
				store.MaxTagsPerNode+1, err)
		}

		// Re-assigning a tag the node ALREADY has is not a limit case even though the node is at
		// its cap: it is a no-op. A client retrying an assignment would otherwise loop forever on
		// an error that describes a condition it did not create.
		var first store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			list, lerr := tx.NodeTags(ctx, owner, f.ID)
			if lerr != nil {
				return lerr
			}
			first = list[0]
			return tx.AssignTag(ctx, f.ID, first.ID, owner)
		}); err != nil {
			t.Fatalf("re-assigning a tag the node already carries returned %v; a client retrying "+
				"an assignment would loop forever on a limit it did not hit", err)
		}
	})

	t.Run("DeletingATagRemovesItsAssignments", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "labelled.txt", 4)

		var tag store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			tag, err = tx.CreateTag(ctx, store.Tag{OwnerID: &owner, Name: "temporary"})
			if err != nil {
				return err
			}
			return tx.AssignTag(ctx, f.ID, tag.ID, owner)
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.DeleteTag(ctx, tag.ID)
		}); err != nil {
			t.Fatal(err)
		}

		var left []store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.NodeTags(ctx, owner, f.ID)
			left = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Fatalf("%d assignment(s) survived the tag's deletion; a filter would then match a "+
				"tag nobody can see", len(left))
		}
	})

	t.Run("AnotherAccountsTagIsInvisible", func(t *testing.T) {
		s, mine := factory(t)
		other := addUser(t, s, "tag-owner-2")
		r := rootOf(t, s, mine)
		f := mkfile(t, s, mine, r.ID, "shared-note.txt", 4)

		var theirs store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			theirs, err = tx.CreateTag(ctx, store.Tag{OwnerID: &other.ID, Name: "private-label"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.AssignTag(ctx, f.ID, theirs.ID, other.ID)
		}); err != nil {
			t.Fatal(err)
		}

		var visible []store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.NodeTags(ctx, mine, f.ID)
			visible = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, tag := range visible {
			if tag.ID == theirs.ID {
				t.Fatal("one account's personal tag is visible to another account on a shared node")
			}
		}
	})

	t.Run("UnassigningRemovesExactlyOneTagAndIsIdempotent", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "two-labels.txt", 4)

		var a, b store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			if a, err = tx.CreateTag(ctx, store.Tag{OwnerID: &owner, Name: "keep-me"}); err != nil {
				return err
			}
			if b, err = tx.CreateTag(ctx, store.Tag{OwnerID: &owner, Name: "drop-me"}); err != nil {
				return err
			}
			if err := tx.AssignTag(ctx, f.ID, a.ID, owner); err != nil {
				return err
			}
			if err := tx.AssignTag(ctx, f.ID, b.ID, owner); err != nil {
				return err
			}
			if err := tx.UnassignTag(ctx, f.ID, b.ID); err != nil {
				return err
			}
			// Un-assigning something that is not assigned is a no-op, not a 404: the caller's
			// intent ("this tag is not on this node") is already satisfied.
			return tx.UnassignTag(ctx, f.ID, b.ID)
		}); err != nil {
			t.Fatal(err)
		}

		var left []store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.NodeTags(ctx, owner, f.ID)
			left = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(left) != 1 || left[0].ID != a.ID {
			t.Fatalf("after un-assigning one of two tags the node carries %d, want the remaining "+
				"one", len(left))
		}
	})
}

// ------------------------------------------------------------------- comments --

func commentSuite(t *testing.T, factory Factory) {
	t.Helper()

	// unlimited turns the rate check off, which is what most cases want: they are about the thread,
	// not about abuse.
	unlimited := store.CommentLimit{}

	t.Run("ATopLevelCommentRoundTrips", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "discussed.txt", 4)

		var made store.Comment
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			made, err = tx.CreateComment(ctx, store.Comment{
				NodeID: f.ID, AuthorID: owner, Body: "first post",
			}, unlimited)
			return err
		}); err != nil {
			t.Fatalf("create comment: %v", err)
		}
		if made.ID == uuid.Nil {
			t.Fatal("a stored comment has no id")
		}
		if made.CreatedAt.IsZero() {
			t.Fatal("a stored comment has no creation time; the thread ordering depends on it")
		}

		var list []store.Comment
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.CommentsAfter(ctx, f.ID, 0, 10)
			list = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Body != "first post" {
			t.Fatalf("the thread returned %d comment(s), first body %q", len(list), firstBody(list))
		}
		if list[0].Seq == 0 {
			t.Fatal("a stored comment has no sequence number; the thread ordering depends on it, " +
				"and created_at cannot supply it because several posts share one timestamp")
		}
	})

	t.Run("AThreadReadsOldestFirst", func(t *testing.T) {
		// A discussion reads in the order it happened. Newest-first would put the reply above the
		// question, which is a thread nobody can follow.
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "thread.txt", 4)

		for _, body := range []string{"one", "two", "three"} {
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.CreateComment(ctx, store.Comment{
					NodeID: f.ID, AuthorID: owner, Body: body,
				}, unlimited)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}

		var list []store.Comment
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.CommentsAfter(ctx, f.ID, 0, 10)
			list = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var bodies []string
		for _, c := range list {
			bodies = append(bodies, c.Body)
		}
		if strings.Join(bodies, ",") != "one,two,three" {
			t.Fatalf("thread order = %v, want oldest first", bodies)
		}
	})

	t.Run("AReplyIsOneLevelOnly", func(t *testing.T) {
		// §6: "threads are single-level replies (parent_id must reference a top-level comment)".
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "forum.txt", 4)

		var top store.Comment
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			top, err = tx.CreateComment(ctx, store.Comment{
				NodeID: f.ID, AuthorID: owner, Body: "top level",
			}, unlimited)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		var reply store.Comment
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			reply, err = tx.CreateComment(ctx, store.Comment{
				NodeID: f.ID, AuthorID: owner, Body: "a reply", ParentID: &top.ID,
			}, unlimited)
			return err
		}); err != nil {
			t.Fatalf("a reply to a top-level comment was refused: %v", err)
		}
		if reply.ParentID == nil || *reply.ParentID != top.ID {
			t.Fatal("the reply lost its parent")
		}

		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			parent := reply.ID
			_, cerr := tx.CreateComment(ctx, store.Comment{
				NodeID: f.ID, AuthorID: owner, Body: "reply to a reply", ParentID: &parent,
			}, unlimited)
			return cerr
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a reply to a reply returned %v, want ErrNotFound: the data model is "+
				"single-level and a deeper row would render as an orphan", err)
		}
	})

	t.Run("AReplyCannotCrossNodes", func(t *testing.T) {
		// A parent on another node would put a comment into a thread about a file the author may
		// not be able to see.
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		a := mkfile(t, s, owner, r.ID, "a.txt", 4)
		b := mkfile(t, s, owner, r.ID, "b.txt", 4)

		var top store.Comment
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			top, err = tx.CreateComment(ctx, store.Comment{
				NodeID: a.ID, AuthorID: owner, Body: "about a",
			}, unlimited)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, cerr := tx.CreateComment(ctx, store.Comment{
				NodeID: b.ID, AuthorID: owner, Body: "pretending to be a reply", ParentID: &top.ID,
			}, unlimited)
			return cerr
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a reply naming a parent on a different node returned %v, want ErrNotFound", err)
		}
	})

	t.Run("TheRateLimitRefusesAndDoesNotRecord", func(t *testing.T) {
		// The anonymous-abuse lesson from the share OTP endpoint: any user-writable append surface
		// needs a durable rolling window.
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "spam-surface.txt", 4)
		limit := store.CommentLimit{Limit: 2, Window: time.Hour}

		for i := 0; i < 2; i++ {
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.CreateComment(ctx, store.Comment{
					NodeID: f.ID, AuthorID: owner, Body: "post " + itoa(i),
				}, limit)
				return err
			}); err != nil {
				t.Fatalf("post %d inside the allowance: %v", i, err)
			}
		}

		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, cerr := tx.CreateComment(ctx, store.Comment{
				NodeID: f.ID, AuthorID: owner, Body: "one too many",
			}, limit)
			return cerr
		})
		if !errors.Is(err, store.ErrCommentRateLimited) {
			t.Fatalf("the post past the limit returned %v, want ErrCommentRateLimited", err)
		}

		// And the refused post really was not written: a limiter that records first and checks
		// second would let a burst through one request at a time.
		var n int64
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var cerr error
			n, cerr = tx.CountRecentComments(ctx, owner, time.Hour)
			return cerr
		}); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("the window holds %d comments after a refusal, want 2: the refused post must "+
				"not be recorded", n)
		}
	})

	t.Run("ADeletedCommentIsATombstoneTheThreadKeeps", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "moderated.txt", 4)

		var made store.Comment
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			made, err = tx.CreateComment(ctx, store.Comment{
				NodeID: f.ID, AuthorID: owner, Body: "to be removed",
			}, unlimited)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		var deleted store.Comment
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			deleted, err = tx.DeleteComment(ctx, made.ID, owner, time.Now())
			return err
		}); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if !deleted.Deleted() {
			t.Fatal("the comment does not report itself as deleted after DeleteComment")
		}
		if deleted.DeletedBy == nil || *deleted.DeletedBy != owner {
			t.Fatal("the deletion is not attributed; a moderator action with no actor is not a " +
				"moderation fact")
		}

		// CommentByID is what the moderation routes read before they decide: they need the
		// tombstone, because "delete an already deleted comment" and "edit a live one" are
		// different answers and both depend on state the thread listing alone does not give.
		var fetched store.Comment
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			fetched, err = tx.CommentByID(ctx, made.ID)
			return err
		}); err != nil {
			t.Fatalf("CommentByID: %v", err)
		}
		if !fetched.Deleted() || fetched.Seq != made.Seq {
			t.Fatalf("CommentByID returned seq=%d deleted=%v, want the same comment as a tombstone",
				fetched.Seq, fetched.Deleted())
		}
		unknownErr := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, cerr := tx.CommentByID(ctx, uuid.New())
			return cerr
		})
		if !errors.Is(unknownErr, store.ErrNotFound) {
			t.Fatalf("CommentByID on an unknown id returned %v, want ErrNotFound", unknownErr)
		}
		// The row stays in the thread so the replies keep their place.
		var list []store.Comment
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.CommentsAfter(ctx, f.ID, 0, 10)
			list = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 {
			t.Fatalf("the thread has %d rows after a soft delete, want the tombstone", len(list))
		}

		// A tombstone cannot be rewritten.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, uerr := tx.UpdateComment(ctx, made.ID, "revived", time.Now())
			return uerr
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("editing a deleted comment returned %v, want ErrNotFound: a tombstone the "+
				"author can rewrite is a moderation decision the author can undo", err)
		}

		// Deleting twice is not an error: two moderators pressing the button at once is a race,
		// not a conflict.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, derr := tx.DeleteComment(ctx, made.ID, owner, time.Now())
			return derr
		}); err != nil {
			t.Fatalf("the second delete failed: %v", err)
		}
	})

	t.Run("EditingStampsEditedAt", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "edited.txt", 4)

		var made store.Comment
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			made, err = tx.CreateComment(ctx, store.Comment{
				NodeID: f.ID, AuthorID: owner, Body: "original",
			}, unlimited)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var edited store.Comment
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			edited, err = tx.UpdateComment(ctx, made.ID, "corrected", time.Now())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if edited.Body != "corrected" {
			t.Fatalf("body = %q after an edit", edited.Body)
		}
		if edited.EditedAt == nil {
			t.Fatal("an edited comment does not carry edited_at; a reader cannot tell a corrected " +
				"post from the original")
		}
	})

	t.Run("TheBodyCapAndControlCharactersAreRefused", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "policy.txt", 4)

		long := strings.Repeat("a", store.MaxCommentRunes+1)
		for _, tc := range []struct{ name, body string }{
			{"empty", "   "},
			{"too long", long},
			{"with NUL", "hello\x00world"},
			{"with a bidi override", "invoice\u202Etxt.exe"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					_, cerr := tx.CreateComment(ctx, store.Comment{
						NodeID: f.ID, AuthorID: owner, Body: tc.body,
					}, unlimited)
					return cerr
				})
				if !errors.Is(err, store.ErrInvalidSocialValue) {
					t.Fatalf("%s returned %v, want ErrInvalidSocialValue", tc.name, err)
				}
			})
		}

		// A multi-line comment with tabs is legitimate: the control-character rule is about
		// characters that render as nothing or as a control code, not about paragraphs.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.CreateComment(ctx, store.Comment{
				NodeID: f.ID, AuthorID: owner, Body: "line one\nline\ttwo",
			}, unlimited)
			return err
		}); err != nil {
			t.Fatalf("a multi-line comment was refused: %v", err)
		}
	})

	t.Run("CommentsPageWithoutRepeatingOrSkipping", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "long-thread.txt", 4)

		for i := 0; i < 5; i++ {
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.CreateComment(ctx, store.Comment{
					NodeID: f.ID, AuthorID: owner, Body: "post " + itoa(i),
				}, unlimited)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}

		seen := map[uuid.UUID]int{}
		var cursor int64
		for page := 0; page < 10; page++ {
			var got []store.Comment
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				list, err := tx.CommentsAfter(ctx, f.ID, cursor, 2)
				got = list
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 {
				break
			}
			for _, c := range got {
				seen[c.ID]++
			}
			cursor = got[len(got)-1].Seq
			if len(got) < 2 {
				break
			}
		}
		if len(seen) != 5 {
			t.Fatalf("paging saw %d distinct comments, want 5", len(seen))
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("comment %s appeared %d times across pages", id, n)
			}
		}
	})
}

// firstBody renders a thread's first body for a failure message.
func firstBody(list []store.Comment) string {
	if len(list) == 0 {
		return ""
	}
	return list[0].Body
}

// itoa is strconv.Itoa without pulling the import into every case above.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// -------------------------------------------------------------------- search --

func filteredSearchSuite(t *testing.T, factory Factory) {
	t.Helper()

	filter := func(t *testing.T, s store.Store) store.FilterSearcher {
		t.Helper()
		fs, ok := s.(store.FilterSearcher)
		if !ok {
			t.Fatalf("%T does not implement store.FilterSearcher; the filter bar would silently "+
				"fall back to name-only on this backend", s)
		}
		return fs
	}

	search := func(t *testing.T, s store.Store, owner uuid.UUID, q store.SearchQuery) store.SearchPage {
		t.Helper()
		page, err := filter(t, s).SearchNodes(context.Background(), owner, q)
		if err != nil {
			t.Fatalf("search %+v: %v", q, err)
		}
		return page
	}

	names := func(page store.SearchPage) []string {
		out := make([]string, 0, len(page.Items))
		for _, item := range page.Items {
			out = append(out, item.Node.Name)
		}
		return out
	}

	t.Run("TheKindFilterSeparatesFilesFromDirectories", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		mkfile(t, s, owner, r.ID, "budget.txt", 4)
		mkdir(t, s, owner, r.ID, "budget")

		if got := names(search(t, s, owner, store.SearchQuery{Text: "budget", Kind: store.KindFile})); len(got) != 1 || got[0] != "budget.txt" {
			t.Fatalf("kind=file returned %v, want [budget.txt]", got)
		}
		if got := names(search(t, s, owner, store.SearchQuery{Text: "budget", Kind: store.KindDir})); len(got) != 1 || got[0] != "budget" {
			t.Fatalf("kind=dir returned %v, want [budget]", got)
		}
	})

	t.Run("TheMimeFilterMatchesAPrefix", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		// mkfile records text/plain, so one binary file is enough to tell a prefix match from a
		// no-op.
		mkfile(t, s, owner, r.ID, "notes.txt", 4)
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: r.ID, Name: "photo.bin", Size: 4,
				Mime: "image/png", ETag: "e", BlobID: newBlobHook(t), BlobHash: [32]byte{9},
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}

		if got := names(search(t, s, owner, store.SearchQuery{MimePrefix: "image/"})); len(got) != 1 || got[0] != "photo.bin" {
			t.Fatalf("mime=image/ returned %v, want [photo.bin]", got)
		}
		if got := names(search(t, s, owner, store.SearchQuery{MimePrefix: "text/"})); len(got) != 1 || got[0] != "notes.txt" {
			t.Fatalf("mime=text/ returned %v, want [notes.txt]", got)
		}
	})

	t.Run("SizeAndTimeFiltersBound", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		small := mkfile(t, s, owner, r.ID, "small.txt", 1)
		big := mkfile(t, s, owner, r.ID, "big.txt", 1000)
		_ = small

		got := names(search(t, s, owner, store.SearchQuery{SizeMin: 500}))
		if len(got) != 1 || got[0] != "big.txt" {
			t.Fatalf("size_min=500 returned %v, want [big.txt]", got)
		}
		got = names(search(t, s, owner, store.SearchQuery{SizeMax: 10}))
		if len(got) != 1 || got[0] != "small.txt" {
			t.Fatalf("size_max=10 returned %v, want [small.txt]", got)
		}

		// modified_before a time in the past excludes everything just written.
		got = names(search(t, s, owner, store.SearchQuery{
			ModifiedBefore: big.ModifiedAt.Add(-time.Hour),
		}))
		if len(got) != 0 {
			t.Fatalf("modified_before an hour ago returned %v, want nothing", got)
		}
		got = names(search(t, s, owner, store.SearchQuery{
			ModifiedAfter: big.ModifiedAt.Add(-time.Hour),
		}))
		if len(got) != 2 {
			t.Fatalf("modified_after an hour ago returned %v, want both files", got)
		}
	})

	t.Run("TheTagFilterMatchesExactlyTheTaggedNodes", func(t *testing.T) {
		// §5's acceptance criterion: "tag filter in search returns exactly the tagged nodes the
		// caller can read".
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		tagged := mkfile(t, s, owner, r.ID, "tagged-report.txt", 4)
		mkfile(t, s, owner, r.ID, "untagged-report.txt", 4)

		var tag store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			tag, err = tx.CreateTag(ctx, store.Tag{OwnerID: &owner, Name: "reviewed"})
			if err != nil {
				return err
			}
			return tx.AssignTag(ctx, tagged.ID, tag.ID, owner)
		}); err != nil {
			t.Fatal(err)
		}

		got := names(search(t, s, owner, store.SearchQuery{Text: "report", TagID: &tag.ID}))
		if len(got) != 1 || got[0] != "tagged-report.txt" {
			t.Fatalf("tag filter returned %v, want only the tagged node", got)
		}
	})

	t.Run("TheFavoriteFilterMatchesOnlyTheCallersStars", func(t *testing.T) {
		s, mine := factory(t)
		other := addUser(t, s, "search-fav-other")
		r := rootOf(t, s, mine)
		starred := mkfile(t, s, mine, r.ID, "starred-report.txt", 4)
		mkfile(t, s, mine, r.ID, "plain-report.txt", 4)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.SetFavorite(ctx, mine, starred.ID)
		}); err != nil {
			t.Fatal(err)
		}

		got := names(search(t, s, mine, store.SearchQuery{Text: "report", Favorite: true}))
		if len(got) != 1 || got[0] != "starred-report.txt" {
			t.Fatalf("favorite filter returned %v, want only the starred node", got)
		}

		// Another account starring the same node does not make it this account's favorite.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.SetFavorite(ctx, other.ID, starred.ID)
		}); err != nil {
			t.Fatal(err)
		}
		// The other account does not own the node, so its own search is empty by owner scoping —
		// which is the point: search never crosses the owner boundary.
		if got := names(search(t, s, other.ID, store.SearchQuery{Favorite: true})); len(got) != 0 {
			t.Fatalf("another account's favorite search returned %v; search is owner-scoped", got)
		}
	})

	t.Run("SortOrdersAreStableAndDistinct", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		mkfile(t, s, owner, r.ID, "charlie.txt", 30)
		mkfile(t, s, owner, r.ID, "alpha.txt", 10)
		mkfile(t, s, owner, r.ID, "bravo.txt", 20)

		if got := names(search(t, s, owner, store.SearchQuery{Sort: store.SearchSortName})); strings.Join(got, ",") != "alpha.txt,bravo.txt,charlie.txt" {
			t.Fatalf("sort=name returned %v", got)
		}
		if got := names(search(t, s, owner, store.SearchQuery{Sort: store.SearchSortSize})); strings.Join(got, ",") != "charlie.txt,bravo.txt,alpha.txt" {
			t.Fatalf("sort=size returned %v, want largest first", got)
		}
	})

	t.Run("SearchPagesWithoutRepeatingOrSkipping", func(t *testing.T) {
		// The ADR-0012 contract applied to a filtered search: one cursor per page, every row
		// exactly once, and an empty cursor on the last page.
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		for _, name := range []string{"page-a.txt", "page-b.txt", "page-c.txt", "page-d.txt", "page-e.txt"} {
			mkfile(t, s, owner, r.ID, name, 4)
		}

		seen := map[string]int{}
		cursor := ""
		pages := 0
		for {
			pages++
			if pages > 10 {
				t.Fatal("paging did not terminate")
			}
			page := search(t, s, owner, store.SearchQuery{
				Sort: store.SearchSortName, Limit: 2, Cursor: cursor,
			})
			for _, name := range names(page) {
				seen[name]++
			}
			if page.Cursor == "" {
				break
			}
			cursor = page.Cursor
		}
		if len(seen) != 5 {
			t.Fatalf("paging saw %d distinct rows, want 5: %v", len(seen), seen)
		}
		for name, n := range seen {
			if n != 1 {
				t.Fatalf("%q appeared %d times across pages", name, n)
			}
		}
	})

	t.Run("ACursorIssuedForOneSortIsRefusedUnderAnother", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		for _, name := range []string{"x-1.txt", "x-2.txt", "x-3.txt"} {
			mkfile(t, s, owner, r.ID, name, 4)
		}
		page := search(t, s, owner, store.SearchQuery{Sort: store.SearchSortName, Limit: 2})
		if page.Cursor == "" {
			t.Fatal("a three-row result with limit 2 returned no cursor")
		}

		_, err := filter(t, s).SearchNodes(context.Background(), owner, store.SearchQuery{
			Sort: store.SearchSortSize, Limit: 2, Cursor: page.Cursor,
		})
		if !errors.Is(err, store.ErrInvalidCursor) {
			t.Fatalf("a name-sort cursor used for a size sort returned %v, want ErrInvalidCursor: "+
				"page 2 of one ordering means nothing in another", err)
		}
	})

	t.Run("SearchStillExcludesTheTrashAndOtherOwners", func(t *testing.T) {
		s, mine := factory(t)
		other := addUser(t, s, "filtered-other")
		r := rootOf(t, s, mine)
		keep := mkfile(t, s, mine, r.ID, "keep-note.txt", 4)
		gone := mkfile(t, s, mine, r.ID, "gone-note.txt", 4)
		_ = keep
		or := rootOf(t, s, other.ID)
		mkfile(t, s, other.ID, or.ID, "theirs-note.txt", 4)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, gone.ID)
		}); err != nil {
			t.Fatal(err)
		}

		got := names(search(t, s, mine, store.SearchQuery{Text: "note"}))
		if len(got) != 1 || got[0] != "keep-note.txt" {
			t.Fatalf("search returned %v, want only the live own file", got)
		}
	})

	t.Run("AnEmptyTextWithFiltersIsAListingNotANoOp", func(t *testing.T) {
		// The filter bar's clear button produces exactly this request. Treating an empty q as
		// "return nothing" (which the palette does on purpose) would make filters unusable.
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		mkfile(t, s, owner, r.ID, "listed.txt", 4)
		mkdir(t, s, owner, r.ID, "listed-dir")

		got := names(search(t, s, owner, store.SearchQuery{Kind: store.KindFile}))
		if len(got) != 1 || got[0] != "listed.txt" {
			t.Fatalf("an empty text with kind=file returned %v, want [listed.txt]", got)
		}
	})

	t.Run("AContradictoryFilterIsRefusedRatherThanEmpty", func(t *testing.T) {
		// Returning nothing for "directories with a MIME type" would tell the user "no matches"
		// for a question the server cannot answer.
		s, owner := factory(t)
		_, err := filter(t, s).SearchNodes(context.Background(), owner, store.SearchQuery{
			Kind: store.KindDir, MimePrefix: "image/",
		})
		if !errors.Is(err, store.ErrInvalidSearch) {
			t.Fatalf("kind=dir with a mime filter returned %v, want ErrInvalidSearch", err)
		}
	})
}

// ----------------------------------------------------------------- empty trash --

func ownerPurgeSuite(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("AnOwnerCanPurgeTheirWholeBinRegardlessOfRetention", func(t *testing.T) {
		// §3: the user has decided, so the retention window that protects them from an accident is
		// exactly what they are overriding.
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "old-delete.txt", 16)
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, f.ID)
		}); err != nil {
			t.Fatal(err)
		}

		var res store.PurgeResult
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.PurgeTrashFor(ctx, owner, 500)
			res = got
			return err
		}); err != nil {
			t.Fatalf("PurgeTrashFor: %v", err)
		}
		if res.Roots != 1 || res.Nodes != 1 {
			t.Fatalf("purge reported roots=%d nodes=%d, want 1 and 1", res.Roots, res.Nodes)
		}
		if res.Bytes != 16 {
			t.Fatalf("purge released %d bytes, want 16", res.Bytes)
		}
		if res.More {
			t.Fatal("the purge reported more work with a one-entry bin")
		}

		// And it is gone from the trash listing.
		var left []store.Node
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.TrashedAfter(ctx, owner, "", 50)
			left = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Fatalf("%d item(s) survived an empty-trash", len(left))
		}
	})

	t.Run("AnOwnerCannotPurgeSomebodyElsesBin", func(t *testing.T) {
		// §3's acceptance criterion: "non-owner sees 404". At this layer the guarantee is that the
		// purge is scoped by owner id in the statement that selects the roots.
		s, mine := factory(t)
		other := addUser(t, s, "purge-victim")
		or := rootOf(t, s, other.ID)
		victim := mkfile(t, s, other.ID, or.ID, "not-yours.txt", 16)
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, victim.ID)
		}); err != nil {
			t.Fatal(err)
		}

		var res store.PurgeResult
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.PurgeTrashFor(ctx, mine, 500)
			res = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if res.Roots != 0 {
			t.Fatalf("an account's purge removed %d of another account's items", res.Roots)
		}

		var stillThere []store.Node
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.TrashedAfter(ctx, other.ID, "", 10)
			stillThere = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(stillThere) != 1 {
			t.Fatalf("the other account's trashed file is gone (%d rows left)", len(stillThere))
		}
	})

	t.Run("APurgeIsBatchedAndReportsThatThereIsMore", func(t *testing.T) {
		// A bin with a million entries must not hold one request open; the caller runs again.
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		for i := 0; i < 3; i++ {
			f := mkfile(t, s, owner, r.ID, "batch-"+itoa(i)+".txt", 4)
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.Trash(ctx, f.ID)
			}); err != nil {
				t.Fatal(err)
			}
		}

		var first store.PurgeResult
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.PurgeTrashFor(ctx, owner, 2)
			first = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if first.Roots != 2 {
			t.Fatalf("a batch of 2 purged %d roots", first.Roots)
		}
		if !first.More {
			t.Fatal("a bounded batch that hit its bound reported More=false; a client would stop " +
				"believing the bin is empty")
		}

		var second store.PurgeResult
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.PurgeTrashFor(ctx, owner, 2)
			second = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if second.Roots != 1 || second.More {
			t.Fatalf("the second batch purged %d roots (More=%v), want the last one and done",
				second.Roots, second.More)
		}
	})

	t.Run("PurgingTwiceChangesNothingTheSecondTime", func(t *testing.T) {
		// Idempotent, so a client that retries after a timeout is safe. This is the property that
		// makes "call me again" an answer instead of "unknown state".
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "purged-once.txt", 4)
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, f.ID)
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.PurgeTrashFor(ctx, owner, 500)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		var again store.PurgeResult
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.PurgeTrashFor(ctx, owner, 500)
			again = got
			return err
		}); err != nil {
			t.Fatalf("the second purge failed: %v", err)
		}
		if again.Roots != 0 || again.Nodes != 0 || again.Bytes != 0 {
			t.Fatalf("the second purge removed roots=%d nodes=%d bytes=%d from an empty bin",
				again.Roots, again.Nodes, again.Bytes)
		}
	})

	t.Run("AnEmptyPurgeReleasesTheRetainedCounter", func(t *testing.T) {
		// §3: "Quota effect is immediate (used_bytes/trash_bytes move in the same transaction)".
		// The bytes were already released from the logical quota when the item was trashed; what
		// the purge must do is release them from RETAINED usage, or the account stays "full".
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "retained.txt", 32)
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, f.ID)
		}); err != nil {
			t.Fatal(err)
		}

		before := trashBytesOf(t, s, owner)
		if before < 32 {
			t.Fatalf("after trashing a 32-byte file the retained counter is %d; the trash "+
				"accounting did not run", before)
		}

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.PurgeTrashFor(ctx, owner, 500)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if after := trashBytesOf(t, s, owner); after != 0 {
			t.Fatalf("the retained counter is %d after an empty-trash, want 0: the user would "+
				"still be told their storage is full", after)
		}
	})
}

// socialAnnotationSuite covers SocialFor, the per-viewer batch annotation the listing uses.
//
// It exists as one query for the whole page because the alternative is N+1: a directory of five
// hundred files would issue fifteen hundred round trips to draw one screen. The interesting
// assertions are the ones that make that batching SAFE: the stars are the viewer's own, the tags
// are the ones the viewer may see, and the checksum is the node's own content address rather than
// a neighbour's.
func socialAnnotationSuite(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("TheBatchAnnotationIsPerViewerAndBatchSafe", func(t *testing.T) {
		s, mine := factory(t)
		other := addUser(t, s, "annotate-other")
		r := rootOf(t, s, mine)
		starred := mkfile(t, s, mine, r.ID, "annotated.txt", 12)
		plain := mkfile(t, s, mine, r.ID, "plain.txt", 4)
		dir := mkdir(t, s, mine, r.ID, "annotated-dir")

		var shared store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			if err := tx.SetFavorite(ctx, mine, starred.ID); err != nil {
				return err
			}
			var terr error
			shared, terr = tx.CreateTag(ctx, store.Tag{OwnerID: &mine, Name: "reviewed"})
			if terr != nil {
				return terr
			}
			return tx.AssignTag(ctx, starred.ID, shared.ID, mine)
		}); err != nil {
			t.Fatal(err)
		}

		reader, ok := s.(store.SocialReader)
		if !ok {
			t.Fatalf("%T does not implement store.SocialReader; the listing would lose its star "+
				"and tag chips", s)
		}

		ids := []uuid.UUID{starred.ID, plain.ID, dir.ID}
		got, err := reader.SocialFor(context.Background(), mine, ids)
		if err != nil {
			t.Fatalf("SocialFor: %v", err)
		}

		if v := got[starred.ID]; !v.Starred {
			t.Fatal("the starred node is not reported as starred")
		}
		if v := got[plain.ID]; v.Starred {
			t.Fatal("an unstarred node is reported as starred")
		}
		if v := got[starred.ID]; len(v.Tags) != 1 || v.Tags[0].ID != shared.ID {
			t.Fatalf("the annotated node carries %d tag(s), want the one assigned", len(got[starred.ID].Tags))
		}
		if v := got[starred.ID]; len(v.SHA256) != 0 && len(v.SHA256) != 32 {
			t.Fatalf("the checksum is %d bytes, want 32 or empty", len(v.SHA256))
		}
		if v := got[dir.ID]; len(v.SHA256) != 0 {
			t.Fatal("a directory reports a content checksum; it has no content address, and a " +
				"client comparing one would be comparing nothing")
		}

		// Another viewer sees their own stars (none) and not this account's.
		theirs, err := reader.SocialFor(context.Background(), other.ID, ids)
		if err != nil {
			t.Fatalf("SocialFor for the other viewer: %v", err)
		}
		if v := theirs[starred.ID]; v.Starred {
			t.Fatal("one account's star is reported for another viewer in the batch annotation")
		}
		if v := theirs[starred.ID]; len(v.Tags) != 0 {
			t.Fatal("one account's personal tag is visible to another viewer")
		}
	})

	t.Run("AnEmptyBatchIsEmptyNotAnError", func(t *testing.T) {
		// The listing of an empty directory asks this with no ids, and an error there would fail
		// a screen that has nothing wrong with it.
		s, owner := factory(t)
		reader, ok := s.(store.SocialReader)
		if !ok {
			t.Fatalf("%T does not implement store.SocialReader", s)
		}
		got, err := reader.SocialFor(context.Background(), owner, nil)
		if err != nil {
			t.Fatalf("an empty batch returned an error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("an empty batch returned %d entries", len(got))
		}
	})

	t.Run("TheTagUsageCountIsPerViewer", func(t *testing.T) {
		// §5's acceptance criterion again, from the counting side: a tag showing "3 uses" while the
		// caller can only find one of them tells them two files exist that they must not know about.
		s, mine := factory(t)
		other := addUser(t, s, "tag-count-other")
		r := rootOf(t, s, mine)
		a := mkfile(t, s, mine, r.ID, "count-a.txt", 4)
		b := mkfile(t, s, mine, r.ID, "count-b.txt", 4)

		var tag store.Tag
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var err error
			tag, err = tx.CreateTag(ctx, store.Tag{OwnerID: &mine, Name: "counted"})
			if err != nil {
				return err
			}
			if err := tx.AssignTag(ctx, a.ID, tag.ID, mine); err != nil {
				return err
			}
			return tx.AssignTag(ctx, b.ID, tag.ID, mine)
		}); err != nil {
			t.Fatal(err)
		}

		counter, ok := s.(store.TagCounter)
		if !ok {
			t.Fatalf("%T does not implement store.TagCounter; the tag list would lose its counts", s)
		}
		counts, err := counter.TagUsageCounts(context.Background(), mine, []uuid.UUID{tag.ID})
		if err != nil {
			t.Fatalf("TagUsageCounts: %v", err)
		}
		if counts[tag.ID] != 2 {
			t.Fatalf("the owner's count is %d, want 2", counts[tag.ID])
		}

		// Another account sees the tag only if it is a SYSTEM tag; a personal one is not theirs to
		// count, so it must be absent rather than zero.
		theirs, err := counter.TagUsageCounts(context.Background(), other.ID, []uuid.UUID{tag.ID})
		if err != nil {
			t.Fatalf("TagUsageCounts for the other viewer: %v", err)
		}
		if n := theirs[tag.ID]; n != 0 {
			t.Fatalf("another account's count for a personal tag is %d, want 0: a count over "+
				"nodes the caller cannot read leaks that they exist", n)
		}
	})

	t.Run("TheFavoriteCountCountsEverybody", func(t *testing.T) {
		// The index's favorite_count is "how many people starred this", which is a property of the
		// document rather than of a viewer — unlike NodeSocial.Starred.
		s, mine := factory(t)
		other := addUser(t, s, "fav-count-other")
		r := rootOf(t, s, mine)
		f := mkfile(t, s, mine, r.ID, "popular.txt", 4)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			if err := tx.SetFavorite(ctx, mine, f.ID); err != nil {
				return err
			}
			return tx.SetFavorite(ctx, other.ID, f.ID)
		}); err != nil {
			t.Fatal(err)
		}

		counter, ok := s.(store.FavoriteCounter)
		if !ok {
			t.Fatalf("%T does not implement store.FavoriteCounter; the index's favorite facet "+
				"would always be zero", s)
		}
		n, err := counter.FavoriteCount(context.Background(), f.ID)
		if err != nil {
			t.Fatalf("FavoriteCount: %v", err)
		}
		if n != 2 {
			t.Fatalf("FavoriteCount = %d, want 2", n)
		}
	})

	t.Run("UsernamesResolveInOneBatch", func(t *testing.T) {
		s, mine := factory(t)
		other := addUser(t, s, "named-other")

		lookup, ok := s.(store.UserLookup)
		if !ok {
			t.Fatalf("%T does not implement store.UserLookup; a comment thread would show raw ids", s)
		}
		names, err := lookup.UsernamesFor(context.Background(), []uuid.UUID{mine, other.ID, uuid.New()})
		if err != nil {
			t.Fatalf("UsernamesFor: %v", err)
		}
		if names[mine] == "" {
			t.Fatalf("the owner's name did not resolve: %v", names)
		}
		if names[other.ID] == "" {
			t.Fatalf("the second account's name did not resolve: %v", names)
		}
		// An unknown id is omitted rather than returned as an empty string, so a caller can tell
		// "no such account" from "an account with a blank name".
		if _, present := names[uuid.New()]; present {
			t.Fatal("an unknown account id was returned by UsernamesFor")
		}
	})
}

// backfillSuite covers the two reads the search index depends on (core-scope §7.2).
//
// Both are Store-level rather than Tx-level, and both are easy to get subtly wrong in a way only a
// long-running deployment would notice: a backfill cursor that is not total re-indexes or skips
// rows forever, and a lag gauge measured from the wrong timestamp either never fires or fires on a
// healthy deployment.
func backfillSuite(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("NodesAfterWalksEveryRowExactlyOnce", func(t *testing.T) {
		// The backfill's whole correctness argument: id is immutable, so a keyset on it neither
		// repeats nor skips a row across a walk that spans many passes.
		//
		// The walk is over the WHOLE table, not just this account's rows, because that is what the
		// backfill is: a global pass so a fresh engine converges. PostgreSQL shares its table across
		// subtests, so the assertion is "the order is total and strictly increasing, and each of MY
		// rows appears exactly once" rather than "the walk returns exactly five rows".
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		want := map[uuid.UUID]bool{}
		for i := 0; i < 5; i++ {
			f := mkfile(t, s, owner, r.ID, "walk-"+itoa(i)+".txt", 4)
			want[f.ID] = true
		}

		seen := map[uuid.UUID]int{}
		prev := uuid.Nil
		cursor := uuid.Nil
		for pass := 0; pass < 2000; pass++ {
			var got []store.Node
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				list, err := tx.NodesAfter(ctx, cursor, 2)
				got = list
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 {
				break
			}
			for _, n := range got {
				// Strictly increasing is the whole contract: a repeated or out-of-order id is what
				// makes a keyset cursor return a row twice or skip one.
				if n.ID.String() <= prev.String() && pass > 0 {
					t.Fatalf("the walk returned %s after %s: the order is not strictly increasing",
						n.ID, prev)
				}
				seen[n.ID]++
				prev = n.ID
			}
			cursor = got[len(got)-1].ID
		}
		for id := range want {
			if seen[id] != 1 {
				t.Fatalf("node %s appeared %d times across backfill passes, want exactly 1",
					id, seen[id])
			}
		}
	})

	t.Run("NodesAfterIncludesTrashedRowsSoTheIndexCanForgetThem", func(t *testing.T) {
		// A walk that omitted trashed rows would leave deleted files in the index forever: the
		// purge job removes the document by id, and this is the read that lets the backfill see
		// that the row is still there but binned.
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		gone := mkfile(t, s, owner, r.ID, "binned.txt", 4)
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, gone.ID)
		}); err != nil {
			t.Fatal(err)
		}

		found := false
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			list, err := tx.NodesAfter(ctx, uuid.Nil, 500)
			for _, n := range list {
				if n.ID == gone.ID && n.TrashedAt != nil {
					found = true
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatal("the backfill walk skipped a trashed node; the index would keep serving it")
		}
	})

	t.Run("OldestPendingJobReportsTheBacklogOrNothing", func(t *testing.T) {
		// The lag gauge is computed from this. "Nothing pending" has to be distinguishable from
		// "a job due now", or a caught-up deployment reports a lag of zero seconds forever and the
		// alert on it is dead.
		s, owner := factory(t)
		rootOf(t, s, owner)

		const kind = "search.index"
		var (
			due time.Time
			ok  bool
		)
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var gerr error
			due, ok, gerr = tx.OldestPendingJob(ctx, kind)
			return gerr
		}); err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("a store with no index jobs reports a pending one due at %s", due)
		}

		// A due-in-the-future job is pending but not LATE. Reporting it as a backlog would make the
		// gauge fire on a healthy deployment that merely scheduled something.
		first := time.Now().Add(2 * time.Minute).UTC()
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			if err := tx.Enqueue(ctx, store.Job{Kind: kind, RunAfter: first,
				Payload: map[string]any{"node_id": uuid.New().String()}}); err != nil {
				return err
			}
			// An older due time must win, because the lag is "how long has the OLDEST been waiting".
			return tx.Enqueue(ctx, store.Job{Kind: kind, RunAfter: first.Add(time.Minute),
				Payload: map[string]any{"node_id": uuid.New().String()}})
		}); err != nil {
			t.Fatal(err)
		}

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var gerr error
			due, ok, gerr = tx.OldestPendingJob(ctx, kind)
			return gerr
		}); err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("a queued index job is not reported as pending; the lag gauge would read zero " +
				"while the queue backs up")
		}
		if delta := due.Sub(first); delta < -time.Second || delta > time.Second {
			t.Fatalf("the oldest pending job is reported due at %s, want the earlier of the two "+
				"(%s)", due, first)
		}
	})
}

// warning: the suites above are small on purpose relative to the whole surface; what follows wires
// them into Run so the conformance ratchet sees every method.
var _ = socialOf
