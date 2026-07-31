// Command newcloud is the single binary: HTTP API, WebDAV endpoint, share pages,
// embedded frontend and the background worker all live in one process.
//
// Subcommands:
//
//	newcloud serve        start the server (default)
//	newcloud migrate      apply pending schema migrations and exit
//	newcloud healthcheck  probe the local server; used by Docker HEALTHCHECK
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
	s3blob "github.com/EdisonJwa/newCloud/internal/blob/s3"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/mail"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/share"
	"github.com/EdisonJwa/newCloud/internal/storage/blobstore"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/storage/tree"
	storepkg "github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
	"github.com/EdisonJwa/newCloud/internal/worker"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "newcloud: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "serve":
		return serve()
	case "migrate":
		return migrate()
	case "healthcheck":
		return healthcheck()
	case "-h", "--help", "help":
		fmt.Printf("newcloud %s\n\nusage: newcloud [serve|migrate|healthcheck]\n", version)
		return nil
	default:
		return fmt.Errorf("unknown command %q (want serve, migrate or healthcheck)", cmd)
	}
}

func configPath() string {
	if p := os.Getenv("NEWCLOUD_CONFIG"); p != "" {
		return p
	}
	return "/etc/newcloud/config.yml"
}

func newLogger() *slog.Logger {
	lvl := slog.LevelInfo
	switch os.Getenv("NEWCLOUD_LOG_LEVEL") {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

func serve() error {
	log := newLogger()

	cfg, err := config.Load(configPath())
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Metadata first: the content store needs it to resolve paths in blob layout,
	// and everything else needs it for identity and authorization.
	meta, closeMeta, err := openMetadata(ctx, log)
	if err != nil {
		return err
	}
	defer closeMeta()

	// Open the content store and run its startup self-checks before accepting a
	// single request. Discovering a split filesystem at 3am while a 100 GiB upload
	// is mid-flight is not an option.
	content, err := openStorage(cfg, meta, log)
	if err != nil {
		return fmt.Errorf("startup self-check failed: %w", err)
	}

	tickets, err := ticket.NewIssuer([]byte(cfg.Auth.SessionSecret))
	if err != nil {
		return err
	}

	provider, err := auth.NewProvider(ctx, cfg)
	if err != nil {
		return err
	}

	mailer, mailErr := mail.New(mail.Config{
		Enabled:            cfg.Mail.Enabled,
		Host:               cfg.Mail.Host,
		Port:               cfg.Mail.Port,
		Username:           cfg.Mail.Username,
		Password:           cfg.Mail.Password,
		From:               cfg.Mail.From,
		ImplicitTLS:        cfg.Mail.ImplicitTLS,
		StartTLS:           cfg.Mail.StartTLS,
		InsecureSkipVerify: cfg.Mail.InsecureSkipVerify,
	}, log)
	if mailErr != nil {
		return mailErr
	}
	if mailer == nil {
		log.Warn("no mail transport configured: email shares will generate codes but cannot deliver them",
			"hint", "set mail.enabled and mail.host to enable delivery")
	}
	if mailer != nil && cfg.Mail.InsecureSkipVerify {
		// A self-signed relay certificate is a legitimate private-network choice, but it
		// removes the only protection against an active attacker on that path.
		log.Warn("mail.insecure_skip_verify is enabled: relay certificates are not verified")
	}

	// Recipient verification codes are minted with the same issuer as session tickets;
	// the share service needs it to run the email-share flow.
	shares := share.New(meta, share.DefaultArgon())
	shares.SetTicketIssuer(tickets)

	deps := server.Deps{
		Config:   cfg,
		Meta:     meta,
		Content:  content,
		Authz:    authz.NewService(&authzAccessor{m: meta}),
		Tickets:  tickets,
		Sessions: auth.NewSessions(tickets, meta, cfg.Auth.SessionTTL),
		AppPass:  auth.NewAppPasswords(meta),
		Shares:   shares,
		Mailer:   mailer,
		Provider: provider,
		Log:      log,
		Version:  version,
	}

	srv, err := server.New(deps)
	if err != nil {
		return err
	}

	// Maintenance runs in-process. The sweep is outbox-style and idempotent, so
	// starting a second replica later adds a second sweeper without changing
	// anything here; the queries are written to tolerate it.
	go worker.New(meta, log).Run(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		// No ReadTimeout/WriteTimeout: large uploads and downloads legitimately
		// take minutes, and a global write timeout would truncate them mid-transfer.
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Server.Addr, "version", version,
			"public_url", cfg.Server.PublicURL, "issuer", cfg.Auth.Issuer)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// openMetadata builds the metadata store: PostgreSQL when configured, otherwise
// in-memory with a loud warning.
func openMetadata(ctx context.Context, log *slog.Logger) (storepkg.Store, func(), error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Warn("no DATABASE_URL set: metadata is in memory and will not survive a restart; " +
			"this is fine for development and unacceptable in production")
		return storepkg.NewMemory(), func() {}, nil
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}

	pg := storepkg.NewPG(pool)
	// Audit writes are chained and transactional; enabling them is not optional
	// for a serving process, because Audit() refuses to commit an unrecorded
	// operation.
	pg.EnableAudit()

	log.Info("metadata store ready", "driver", "postgres")
	return pg, pool.Close, nil
}

// openS3Storage builds the content store on top of an S3-compatible bucket.
//
// The layout is still the content-addressed one: the bucket holds blobs keyed by hash, and the
// tree layout is rejected in openStorage because it is a filesystem mirror by definition. That
// keeps the two axes independent exactly as they are for local disk.
func openS3Storage(cfg config.Config, meta storepkg.Store, log *slog.Logger) (layout.ContentStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s3cfg := cfg.Storage.S3
	store, err := s3blob.New(ctx, s3blob.Config{
		Endpoint:  s3cfg.Endpoint,
		Bucket:    s3cfg.Bucket,
		Region:    s3cfg.Region,
		AccessKey: s3cfg.AccessKey,
		SecretKey: s3cfg.SecretKey,
		Secure:    s3cfg.UseTLS(),
		Prefix:    s3cfg.Prefix,
	})
	if err != nil {
		return nil, err
	}

	log.Info("storage ready",
		"backend", "s3", "layout", string(layout.ModeBlob),
		"endpoint", s3cfg.Endpoint, "bucket", s3cfg.Bucket,
		"deduplicated", true, "inspectable", false)

	adapter, err := blobstore.New(store, &resolverAdapter{r: meta.Resolver()})
	if err != nil {
		return nil, err
	}
	// Uploads stage on local disk even when the bytes land in a bucket: the hash is needed
	// before the object can be named.
	adapter = adapter.WithSpoolDir(cfg.Storage.Local.Root)
	return adapter, nil
}

// openStorage builds the content store for the configured backend and layout.
//
// The two axes are independent:
//
//	backend  where the bytes physically live   (local disk, S3)
//	layout   how they are arranged there       (content-addressed, mirrored tree)
//
// Only local+blob and local+tree are implemented; S3 is rejected with a clear
// message rather than half-working.
func openStorage(cfg config.Config, meta storepkg.Store, log *slog.Logger) (layout.ContentStore, error) {
	if cfg.Storage.Layout == config.LayoutTree {
		if cfg.Storage.Backend != "local" {
			return nil, fmt.Errorf("layout %q requires the local backend", cfg.Storage.Layout)
		}
		s, err := tree.New(cfg.Storage.Local.Root)
		if err != nil {
			return nil, err
		}
		caps := s.Capabilities()
		log.Info("storage ready",
			"backend", cfg.Storage.Backend, "layout", string(s.Mode()),
			"root", cfg.Storage.Local.Root,
			"deduplicated", caps.Deduplicated, "inspectable", caps.DirectFSInspect)
		return s, nil
	}

	if cfg.Storage.Backend == "s3" {
		return openS3Storage(cfg, meta, log)
	}

	if cfg.Storage.Backend != "local" {
		return nil, fmt.Errorf("storage backend %q is not supported; use \"local\" or \"s3\"", cfg.Storage.Backend)
	}

	ls, err := local.New(cfg.Storage.Local.Root)
	if err != nil {
		return nil, err
	}
	// Prove a blob can be published with a single rename before serving traffic.
	// Splitting tmp and content across filesystems degrades that to a full copy,
	// which at scale is an outage rather than a slowdown.
	if err := ls.CheckSameFilesystem(); err != nil {
		return nil, err
	}

	log.Info("storage ready",
		"backend", cfg.Storage.Backend, "layout", string(layout.ModeBlob),
		"root", cfg.Storage.Local.Root, "deduplicated", true, "inspectable", false)

	// Stage uploads inside the blob root: a container may have a read-only /tmp, and
	// staging on another filesystem would turn the publish into a full copy.
	adapter, err := blobstore.New(ls, &resolverAdapter{r: meta.Resolver()})
	if err != nil {
		return nil, err
	}
	return adapter.WithSpoolDir(filepath.Join(cfg.Storage.Local.Root, "tmp")), nil
}

// authzAccessor adapts the metadata store to the authorizer's needs.
//
// It is the only place that translates store errors into authz errors, so the
// authorizer never has to import the store.
type authzAccessor struct{ m storepkg.Store }

func (a *authzAccessor) LoadNode(id uuid.UUID) (authz.Node, error) {
	var n storepkg.Node
	err := a.m.WithTx(context.Background(), func(ctx context.Context, tx storepkg.Tx) error {
		got, gerr := tx.Node(ctx, id)
		n = got
		return gerr
	})
	if err != nil {
		if errors.Is(err, storepkg.ErrNotFound) {
			return authz.Node{}, authz.ErrNotFound
		}
		return authz.Node{}, err
	}
	return authz.Node{ID: n.ID, OwnerID: n.OwnerID, ParentID: n.ParentID}, nil
}

func (a *authzAccessor) OwnerGroupPerm(actorID, nodeID uuid.UUID) (authz.Perm, bool, error) {
	// Group-shared spaces are not implemented yet. Returning "no grant" is the
	// correct fail-closed answer: a stub that returned read access would silently
	// widen every user's permissions.
	return 0, false, nil
}

func (a *authzAccessor) ShareForActor(actorID, nodeID uuid.UUID) (*authz.ShareGrant, error) {
	// Explicit shares to an account are resolved by the share handlers, not here.
	return nil, nil
}

// resolverAdapter bridges store.Resolver onto blobstore.NodeResolver.
//
// The two interfaces differ only in which package owns the node type. Keeping them
// separate is deliberate: the store must not import the storage package, or the
// dependency graph becomes a cycle as soon as storage wants a metadata type back.
type resolverAdapter struct{ r storepkg.Resolver }

func (a *resolverAdapter) HashFor(ctx context.Context, owner, p string) (blob.Hash, bool, error) {
	h, ok, err := a.r.HashFor(ctx, owner, p)
	return blob.Hash(h), ok, err
}

func (a *resolverAdapter) Record(ctx context.Context, owner, p string, h blob.Hash, isDir bool) error {
	return a.r.Record(ctx, owner, p, [32]byte(h), isDir)
}

func (a *resolverAdapter) Forget(ctx context.Context, owner, p string) error {
	return a.r.Forget(ctx, owner, p)
}

func (a *resolverAdapter) IsDir(ctx context.Context, owner, p string) (bool, bool, error) {
	return a.r.IsDir(ctx, owner, p)
}

func (a *resolverAdapter) Children(ctx context.Context, owner, dir string, limit int) ([]layout.Node, error) {
	nodes, err := a.r.Children(ctx, owner, dir, limit)
	if err != nil {
		return nil, err
	}
	out := make([]layout.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, layout.Node{Owner: owner, Path: n.Path, IsDir: n.IsDir, Size: n.Size})
	}
	return out, nil
}

// migrate applies the embedded schema.
func migrate() error {
	log := newLogger()

	if _, err := config.Load(configPath()); err != nil {
		return err
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("migrate: DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := storepkg.ApplySchema(ctx, pool); err != nil {
		return err
	}
	log.Info("schema up to date")
	return nil
}

// healthcheck probes the local server for Docker's HEALTHCHECK directive.
func healthcheck() error {
	addr := os.Getenv("NEWCLOUD_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if len(addr) > 0 && addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: status %d", resp.StatusCode)
	}
	return nil
}
