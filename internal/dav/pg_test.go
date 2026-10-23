package dav

// The COPY overwrite path exercised against REAL PostgreSQL, in both layouts.
//
// Why this file exists: every other DAV test runs on the in-memory metadata store, and the overwrite
// swap depends on transaction semantics the two backends implement separately — two renames, a trash
// and two audit rows inside ONE transaction, where a partial commit would leave the destination
// named after a node whose bytes are elsewhere. The memory store is a map swap; the PostgreSQL path
// writes rows under a partial unique index on (parent_id, name) WHERE trashed_at IS NULL. "It passed
// on the map" is not evidence about that.
//
// Skipped unless TEST_DATABASE_URL is set, the same contract the store package uses. Point it at a
// DISPOSABLE database: the schema is applied idempotently and rows are created per run, so it must
// not be a database whose contents matter.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/storage/blobstore"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/storage/tree"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// pgEnv is a DAV handler over real PostgreSQL and one of the two content layouts.
type pgEnv struct {
	h      *Handler
	meta   store.Store
	pool   *pgxpool.Pool
	user   store.User
	secret string
}

func (e *pgEnv) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.SetBasicAuth(e.user.Username, e.secret)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *pgEnv) davPath(rel string) string {
	return "https://cloud.example.test/dav/" + e.user.Username + "/" + strings.TrimPrefix(rel, "/")
}

// newPGEnv builds the production combination: PostgreSQL metadata plus the requested content layout.
func newPGEnv(t *testing.T, mode layout.Mode) *pgEnv {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping the PostgreSQL overwrite test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.ApplySchema(ctx, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	meta := store.NewPG(pool)
	meta.EnableAudit()

	// A fresh account per run, so a database reused between runs does not accumulate state that
	// changes the answer.
	id := uuid.New()
	username := "dav" + id.String()[:8]
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, username, email, role) VALUES ($1, $2, $3, 'user')`,
		id, username, username+"@example.test"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := meta.EnsureRoot(ctx, id); err != nil {
		t.Fatalf("ensure root: %v", err)
	}

	var user store.User
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UserByID(ctx, id)
		user = u
		return uerr
	}); err != nil {
		t.Fatalf("read back the user: %v", err)
	}

	cfg := config.Default()
	cfg.Server.PublicURL = "https://cloud.example.test"
	cfg.Server.UserContentURL = "https://content.example.test"
	cfg.Limits.MaxProfindChildren = 1000

	var content layout.ContentStore
	if mode == layout.ModeTree {
		ts, terr := tree.New(t.TempDir())
		if terr != nil {
			t.Fatalf("tree store: %v", terr)
		}
		t.Cleanup(func() { _ = ts.Close() })
		content = ts
	} else {
		blobs, berr := local.New(t.TempDir())
		if berr != nil {
			t.Fatalf("blob store: %v", berr)
		}
		bs, berr := blobstore.New(blobs, &resolverAdapter{r: meta.Resolver()})
		if berr != nil {
			t.Fatalf("blobstore: %v", berr)
		}
		content = bs
	}

	appPass := auth.NewAppPasswords(meta)
	secret, _, err := appPass.Issue(ctx, id, "pg-test", "rw", 0)
	if err != nil {
		t.Fatalf("issue app password: %v", err)
	}

	h := New(cfg, meta, content, authz.NewService(&authzAccessor{m: meta}), appPass)
	return &pgEnv{h: h, meta: meta, pool: pool, user: user, secret: secret}
}

// TestCopyOverwriteOnPostgreSQL is the PRG-001 scenario on the backend production actually uses.
func TestCopyOverwriteOnPostgreSQL(t *testing.T) {
	for _, mode := range []layout.Mode{layout.ModeBlob, layout.ModeTree} {
		t.Run(string(mode), func(t *testing.T) {
			e := newPGEnv(t, mode)

			if rec := e.do(t, "MKCOL", e.davPath("src"), "", nil); rec.Code != http.StatusCreated {
				t.Fatalf("mkcol src: %d", rec.Code)
			}
			if rec := e.do(t, http.MethodPut, e.davPath("src/new.txt"),
				"the replacement", nil); rec.Code != http.StatusCreated {
				t.Fatalf("seed src: %d", rec.Code)
			}
			if rec := e.do(t, "MKCOL", e.davPath("dst"), "", nil); rec.Code != http.StatusCreated {
				t.Fatalf("mkcol dst: %d", rec.Code)
			}
			if rec := e.do(t, http.MethodPut, e.davPath("dst/keep.txt"),
				"the original bytes", nil); rec.Code != http.StatusCreated {
				t.Fatalf("seed dst: %d", rec.Code)
			}

			rec := e.do(t, "COPY", e.davPath("src"), "",
				map[string]string{"Destination": e.davPath("dst")})
			if rec.Code != http.StatusNoContent {
				t.Fatalf("%s: COPY overwrite on PostgreSQL = %d, want 204 (%s). The swap runs two "+
					"renames, a trash and two audit rows in one transaction; a partial commit would "+
					"leave the destination named after a node whose bytes are elsewhere.",
					mode, rec.Code, rec.Body.String())
			}

			// The replacement is live with its bytes.
			got := e.do(t, http.MethodGet, e.davPath("dst/new.txt"), "", nil)
			if got.Code != http.StatusOK {
				t.Fatalf("%s: GET dst/new.txt = %d, want 200", mode, got.Code)
			}
			if got.Body.String() != "the replacement" {
				t.Fatalf("%s: dst/new.txt = %q", mode, got.Body.String())
			}

			// The original is gone from the live tree and present in the trash.
			if old := e.do(t, http.MethodGet, e.davPath("dst/keep.txt"), "", nil); old.Code == http.StatusOK {
				t.Fatalf("%s: the previous destination survived in the live tree", mode)
			}
			var trashed []store.Node
			if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				items, terr := tx.Trashed(ctx, e.user.ID, 50)
				trashed = items
				return terr
			}); err != nil {
				t.Fatalf("%s: read trash: %v", mode, err)
			}
			if len(trashed) == 0 {
				t.Fatalf("%s: the replaced destination is not in the trash, so the previous version "+
					"is not recoverable", mode)
			}
		})
	}
}

// TestTheOverwriteIsAuditedOnPostgreSQL checks the audit trail explicitly.
//
// A committed swap with no log entry is the state an operator cannot explain afterwards, and the
// audit's PRG-005 called out that the intermediate commits had none. Both rows are now written
// inside the transaction that performs the switch, so "the copy happened" and "the copy was
// recorded" cannot diverge.
func TestTheOverwriteIsAuditedOnPostgreSQL(t *testing.T) {
	e := newPGEnv(t, layout.ModeBlob)

	if rec := e.do(t, http.MethodPut, e.davPath("a.txt"), "new", nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed a: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, e.davPath("b.txt"), "old", nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed b: %d", rec.Code)
	}
	if rec := e.do(t, "COPY", e.davPath("a.txt"), "",
		map[string]string{"Destination": e.davPath("b.txt")}); rec.Code != http.StatusNoContent {
		t.Fatalf("copy: %d (%s)", rec.Code, rec.Body.String())
	}

	var writes, deletes int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE action = 'file.write'),
		        count(*) FILTER (WHERE action = 'file.delete')
		   FROM audit.events
		  WHERE actor_user_id = $1`, e.user.ID).Scan(&writes, &deletes); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if writes < 2 {
		t.Fatalf("file.write audit rows = %d, want at least 2 (the two seeds plus the replacement)",
			writes)
	}
	if deletes < 1 {
		t.Fatalf("file.delete audit rows = %d, want at least 1 for the replaced destination. A "+
			"committed overwrite with no delete entry is state an operator cannot explain.", deletes)
	}
}
