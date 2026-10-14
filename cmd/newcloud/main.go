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
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/auditverify"
	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
	s3blob "github.com/EdisonJwa/newCloud/internal/blob/s3"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/mail"
	"github.com/EdisonJwa/newCloud/internal/metrics"
	"github.com/EdisonJwa/newCloud/internal/password"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/share"
	"github.com/EdisonJwa/newCloud/internal/storage/blobstore"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/storage/tree"
	storepkg "github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
	"github.com/EdisonJwa/newCloud/internal/upload"
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
	case "audit":
		return auditCommand(args[1:])
	case "breakglass":
		return breakglassCommand(args[1:])
	case "export":
		return exportCommand(args[1:])
	case "drill":
		return drillCommand(args[1:])
	case "-h", "--help", "help":
		fmt.Printf("newcloud %s\n\nusage: newcloud [serve|migrate|healthcheck|audit verify|breakglass hash|drill]\n", version)
		return nil
	default:
		return fmt.Errorf("unknown command %q (want serve, migrate, healthcheck, audit verify, breakglass hash or drill)", cmd)
	}
}

// auditCommand runs the audit sub-commands.
//
// It exists because the hash chain is only worth having if something reads it back
// and complains. Before this, `VerifyAuditChain` was referenced by tests alone, so
// tampering would have gone unnoticed in production forever.
func auditCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("audit: missing sub-command (want `audit verify`)")
	}
	switch args[0] {
	case "verify":
		return auditVerify(args[1:])
	case "prune":
		return auditPrune(args[1:])
	default:
		return fmt.Errorf("audit: unknown sub-command %q (want verify or prune)", args[0])
	}
}

// auditPrune archives and then removes events older than the retention window.
//
// This is a command rather than a background job because its failure mode is destructive and its
// cost is proportional to history: an operator schedules it, watches it once, and knows what it did.
// The archive file is written BEFORE anything is deleted, and its path is recorded in the prune mark,
// so "where did last year's audit log go" has an answer inside the log itself.
func auditPrune(args []string) error {
	log := newLogger()

	flags := flag.NewFlagSet("audit prune", flag.ContinueOnError)
	days := flags.Int("days", 0, "retain this many days (default: audit.retention_days)")
	before := flags.String("before", "", "remove events older than this date (YYYY-MM-DD)")
	dryRun := flags.Bool("dry-run", false, "report what would be removed and stop")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	cfg, err := config.Load(configPath())
	if err != nil {
		return err
	}
	cutoff, err := pruneCutoff(cfg, *days, *before, time.Now().UTC())
	if err != nil {
		return err
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("audit prune: DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	pg := storepkg.NewPG(pool)

	if *dryRun {
		n, oldest, newest, derr := pg.AuditBefore(ctx, cutoff)
		if derr != nil {
			return derr
		}
		log.Info("audit prune (dry run): nothing was removed",
			"cutoff", cutoff.Format(time.RFC3339), "events", n,
			"first_event", oldest, "last_event", newest)
		return nil
	}

	// Verify BEFORE removing anything. The store's own boundary check only catches a break at the
	// event the mark would carry; a rewrite in the middle of the range passes it. Archiving part of
	// a log that does not verify would make the archive the only copy of a record nobody has
	// checked, and the mark would vouch for it.
	mark, err := pg.LatestPruneMark(ctx)
	if err != nil {
		return fmt.Errorf("audit prune: read prune marks: %w", err)
	}
	report, err := auditverify.RunFrom(ctx, pg, 1000, mark.ThroughID, verifyStart(mark))
	if err != nil {
		return fmt.Errorf("audit prune: verify before pruning: %w", err)
	}
	if !report.OK() {
		return fmt.Errorf("audit prune: refusing to prune a log that does not verify: %v "+
			"(broken at event %d). Repair or investigate the chain first; a prune would archive "+
			"history that cannot be shown to be intact", report.Err, report.BrokenAt)
	}

	archivePath, err := archiveAudit(ctx, pg, cfg, cutoff)
	if err != nil {
		return err
	}
	res, err := pg.PruneAuditBefore(ctx, cutoff, archivePath)
	if err != nil {
		if errors.Is(err, storepkg.ErrNoAuditToPrune) {
			log.Info("audit prune: nothing older than the cutoff", "cutoff", cutoff.Format(time.RFC3339))
			return nil
		}
		return err
	}
	log.Info("audit pruned",
		"events", res.Mark.Events, "through_event", res.Mark.ThroughID,
		"archive", res.Mark.ArchivePath, "cutoff", cutoff.Format(time.RFC3339))
	return nil
}

// archiveAudit writes the events that are about to be removed, and returns the file's path.
//
// The file is written and closed BEFORE the prune transaction starts, and fsync'd, because the mark
// records its path as the place the history went. A path that points at a half-written file would be
// worse than no path: it would look like an archive.
func archiveAudit(ctx context.Context, pg *storepkg.PG, cfg config.Config, cutoff time.Time) (string, error) {
	// The destination defaults next to the anchors, which is the one place an operator is already
	// expected to send data somewhere the database cannot rewrite.
	dir := cfg.Audit.ArchiveDir
	if dir == "" {
		dir = "audit-archive"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("audit prune: create archive directory: %w", err)
	}
	events, err := pg.AuditEventsBefore(ctx, cutoff)
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return "", storepkg.ErrNoAuditToPrune
	}

	name := filepath.Join(dir, "audit-"+cutoff.UTC().Format("20060102T150405Z")+".jsonl")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("audit prune: create archive: %w", err)
	}
	enc := json.NewEncoder(f)
	for i := range events {
		// The stored hash is included on purpose: an archive is evidence, and evidence whose
		// digests were recomputed at read time proves only that the reader is consistent.
		if err := enc.Encode(events[i]); err != nil {
			f.Close()
			return "", fmt.Errorf("audit prune: write archive: %w", err)
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", fmt.Errorf("audit prune: sync archive: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("audit prune: close archive: %w", err)
	}
	return name, nil
}

// pruneCutoff resolves the retention window from the flags and the configuration.
func pruneCutoff(cfg config.Config, days int, before string, now time.Time) (time.Time, error) {
	if before != "" {
		t, err := time.Parse(time.RFC3339, before+"T00:00:00Z")
		if err != nil {
			return time.Time{}, fmt.Errorf("audit prune: --before must be YYYY-MM-DD: %w", err)
		}
		return t, nil
	}
	if days <= 0 {
		days = cfg.Audit.RetentionDays
	}
	if days <= 0 {
		return time.Time{}, errors.New("audit prune: no retention window: set audit.retention_days " +
			"or pass --days/--before. Refusing to guess, because the guess deletes history")
	}
	return now.AddDate(0, 0, -days), nil
}

// auditVerify walks the chain and prints a report.
//
// The exit status is the contract: 0 means intact, 1 means broken. That is what
// makes it usable from cron or CI without parsing output.
func auditVerify(args []string) error {
	log := newLogger()

	flags := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	batch := flags.Int("batch", 1000, "events to read per round trip")
	jsonOut := flags.Bool("json", false, "emit the report as JSON")
	exportPath := flags.String("export", "",
		"verify an exported file (JSON export, prune archive or CSV) instead of the database")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// `-h` asks for usage; it is not a failure.
			return nil
		}
		return err
	}

	// A file is verified on its own terms: each event carries its own digest and its predecessor's,
	// so the check needs no database at all. That is what makes an export checkable on the machine
	// it is going to, not only on the one it came from.
	if *exportPath != "" {
		events, lerr := auditverify.LoadFile(*exportPath)
		if lerr != nil {
			return lerr
		}
		report := auditverify.VerifyEvents(events)
		logVerifyReport(log, report)
		if !report.OK() {
			return fmt.Errorf("audit verify: export is broken: %w", report.Err)
		}
		log.Info("export verified", "file", *exportPath, "events", report.Events)
		return nil
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("audit verify: DATABASE_URL is required (or pass --export)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	pg := storepkg.NewPG(pool)

	// Start from the newest archive boundary, if the log has been pruned. Verifying from genesis
	// would report every pruned log as broken — the same answer it gives for tampering — so the
	// boundary and the hash the archive ended at are what the check must begin with.
	mark, merr := pg.LatestPruneMark(ctx)
	if merr != nil {
		return fmt.Errorf("audit verify: read prune marks: %w", merr)
	}
	report, err := auditverify.RunFrom(ctx, pg, *batch, mark.ThroughID,
		verifyStart(mark))
	if mark.ThroughID > 0 {
		log.Info("verifying from the newest archive boundary",
			"through_event", mark.ThroughID, "archived_events", mark.Events,
			"archive", mark.ArchivePath)
	}
	if err != nil {
		return err
	}

	if *jsonOut {
		out := map[string]any{
			"events":    report.Events,
			"ok":        report.OK(),
			"head":      hex.EncodeToString(report.Head),
			"broken_at": report.BrokenAt,
		}
		if report.FirstEvent != nil {
			out["first_event"] = report.FirstEvent.UTC().Format(time.RFC3339)
		}
		if report.LastEvent != nil {
			out["last_event"] = report.LastEvent.UTC().Format(time.RFC3339)
		}
		if report.Err != nil {
			out["error"] = report.Err.Error()
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	} else {
		logVerifyReport(log, report)
	}

	if !report.OK() {
		return fmt.Errorf("audit verify: chain is broken: %w", report.Err)
	}
	return nil
}

func formatOptional(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
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

	warnAboutUnusedConfig(log, cfg)

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
	content, blobStore, err := openStorage(cfg, meta, log)
	if err != nil {
		return fmt.Errorf("startup self-check failed: %w", err)
	}

	tickets, err := ticket.NewIssuer([]byte(cfg.Auth.SessionSecret))
	if err != nil {
		return err
	}

	// OIDC discovery is attempted at startup but is NOT a startup requirement (ID-07).
	//
	// Failing here used to abort the process, which took down WebDAV, app-password
	// authentication and every already-issued browser session — none of which involve the
	// identity provider. A single-sign-on dependency had become an uptime dependency for the
	// whole platform. Now the server starts and only new logins degrade, until discovery
	// succeeds.
	providers := auth.NewProviderHolder()
	provider, provErr := auth.NewProvider(ctx, cfg)
	if provErr != nil {
		log.Warn("identity provider unreachable at startup: starting anyway; "+
			"existing sessions, app passwords and WebDAV continue to work, new logins will "+
			"fail until it is reachable",
			"issuer", cfg.Auth.Issuer, "error", provErr)
	} else {
		providers.Set(provider)
		log.Info("identity provider discovered", "issuer", cfg.Auth.Issuer)
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

	// Resumable uploads stage on local disk even when the bytes land in a bucket: a chunk has
	// to be written somewhere before it can be hashed and published.
	uploadDir := cfg.Storage.UploadDir
	if uploadDir == "" {
		uploadDir = filepath.Join(os.TempDir(), "newcloud-uploads")
		log.Info("upload staging directory is not configured; using the temporary directory",
			"dir", uploadDir, "hint", "set storage.upload_dir on a real deployment")
	}
	uploads, err := upload.NewManager(uploadDir, cfg.Limits.UploadSessionTTL(), 4)
	if err != nil {
		return err
	}
	uploads.SetMaxPendingBytes(cfg.Limits.MaxPendingUploadBytes)
	if err != nil {
		return err
	}
	defer uploads.Close()

	// Expired sessions hold staging files, so the sweep has to run periodically rather than
	// only at startup.
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n := uploads.Sweep(); n > 0 {
					log.Info("discarded expired upload sessions", "count", n)
				}
			}
		}
	}()

	// One registry shared by the server and the worker, so a scrape sees the request
	// counters and the queue depth in the same document.
	reg := metrics.New()

	deps := server.Deps{
		Config:  cfg,
		Meta:    meta,
		Content: content,
		// Set for the addressable layout only: reading a historical version needs an address,
		// and openStorage returns nil here for the tree layout.
		Blobs:    blobStore,
		Settings: server.NewSettingsCache(),
		// The split is decided by configuration: a content address means the application origin must
		// stop serving user bytes, so the route moves rather than being duplicated.
		ContentSplit: cfg.Server.UserContentAddr != "",
		Authz:        authz.NewService(&authzAccessor{m: meta}),
		Tickets:      tickets,
		Sessions:     auth.NewSessions(tickets, meta, cfg.Auth.SessionTTL),
		AppPass:      auth.NewAppPasswords(meta),
		Shares:       shares,
		Mailer:       mailer,
		Provider:     provider,
		Providers:    providers,
		Uploads:      uploads,
		Metrics:      reg,
		Log:          log,
		Version:      version,
	}

	srv, err := server.New(deps)
	if err != nil {
		return err
	}

	// Maintenance runs in-process. The sweep is outbox-style and idempotent, so
	// starting a second replica later adds a second sweeper without changing
	// anything here; the queries are written to tolerate it.
	wk := worker.New(meta, log).WithRetention(cfg.Limits.VersionRetention(), cfg.Limits.TrashRetention())
	wk.WithMetrics(reg)
	// §9.3: 清理未完成 Multipart Upload。Partial uploads are not objects, so no listing, no HEAD and
	// no usage count can see them; without this pass they persist until the bucket is emptied by
	// hand. Enabled only when the backend supports it — the optional interface exists so a backend
	// that cannot clean up does not have to pretend.
	if cleaner, ok := blobStore.(blob.UploadCleaner); ok {
		grace := cfg.Limits.UploadCleanupGrace()
		if grace > 0 {
			wk.WithUploadCleanup(cleaner, 0, grace)
			log.Info("partial-upload cleanup enabled", "grace", grace)
		} else {
			log.Warn("partial-upload cleanup disabled: upload_cleanup_grace_hours is not set, so " +
				"abandoned multipart parts and crash-left temp files accumulate and keep costing storage")
		}
	}
	if n := cfg.Limits.BlobScrubPerPass; n > 0 {
		// The scrub is the only thing that reads stored content back. Without it, a blob
		// corrupted in place is invisible until a user opens the file — or until a restore
		// drill, months later, reports it as a backup problem.
		wk.WithBlobScrub(blobStore, n)
		if iv := cfg.Limits.BlobScrubInterval(); iv > 0 {
			wk.WithScrubInterval(iv)
		}
		log.Info("blob integrity scrub enabled", "blobs_per_pass", n)
	} else {
		// Named at startup for the same reason as audit anchoring: an operator who believes
		// the scrub is running and one who never enabled it look identical from outside.
		log.Warn("blob integrity scrub is disabled: silent corruption will not be detected",
			"hint", "set limits.blob_scrub_per_pass to the number of blobs to verify per cycle")
	}
	if cfg.Audit.AnchorDir != "" {
		if err := wk.EnableAnchoring(cfg.Audit.AnchorDir, meta); err != nil {
			log.Error("audit anchoring is configured but unusable", "dir", cfg.Audit.AnchorDir, "error", err)
			return err
		}
		log.Info("audit chain anchoring enabled", "dir", cfg.Audit.AnchorDir)
	} else {
		// Say so explicitly: an unanchored chain cannot detect its own truncation, and
		// an operator should learn that from the startup log rather than from an
		// incident review.
		log.Warn("audit anchoring is disabled: a truncated log will still verify",
			"hint", "set audit.anchor_dir to a location with different credentials from the database")
	}
	go wk.Run(ctx)

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
func openS3Storage(cfg config.Config, meta storepkg.Store, log *slog.Logger) (layout.ContentStore, blob.Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s3cfg := cfg.Storage.S3
	store, err := s3blob.New(ctx, s3blob.Config{
		Endpoint:    s3cfg.Endpoint,
		Bucket:      s3cfg.Bucket,
		Region:      s3cfg.Region,
		AccessKey:   s3cfg.AccessKey,
		SecretKey:   s3cfg.SecretKey,
		Secure:      s3cfg.UseTLS(),
		Prefix:      s3cfg.Prefix,
		SSEMode:     s3cfg.SSE,
		SSEKMSKeyID: s3cfg.SSEKMSKeyID,
	})
	if err != nil {
		return nil, nil, err
	}

	// Say which encryption is in force. A deployment that believes it has encryption at rest and
	// cannot see the setting in its own startup log is exactly the deployment that finds out
	// otherwise during an audit.
	sse := s3cfg.SSE
	if sse == "" {
		sse = "bucket-policy"
	}
	log.Info("storage ready",
		"backend", "s3", "layout", string(layout.ModeBlob),
		"endpoint", s3cfg.Endpoint, "bucket", s3cfg.Bucket,
		"encryption", sse,
		"deduplicated", true, "inspectable", false)

	adapter, err := blobstore.New(store, &resolverAdapter{r: meta.Resolver()})
	if err != nil {
		return nil, nil, err
	}
	// Uploads stage on local disk even when the bytes land in a bucket: the hash is needed
	// before the object can be named.
	adapter = adapter.WithSpoolDir(cfg.Storage.Local.Root)
	return adapter, store, nil
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
// openStorage builds the content store and, when the layout is content-addressed, the underlying
// blob store as well.
//
// The second return value exists for the integrity scrub, which reads blobs back by their hash.
// layout.ContentStore addresses files by owner+path, which is the wrong question for a scrub: it
// would verify the files that happen to be easy to name and miss rows whose bytes are gone. The
// blob store addresses by content hash, which is exactly what the scrub compares against. It is nil
// for the tree layout, where the naming is a path rather than a hash and the check does not apply.
func openStorage(cfg config.Config, meta storepkg.Store, log *slog.Logger) (layout.ContentStore, blob.Store, error) {
	if cfg.Storage.Layout == config.LayoutTree {
		if cfg.Storage.Backend != "local" {
			return nil, nil, fmt.Errorf("layout %q requires the local backend", cfg.Storage.Layout)
		}
		s, err := tree.New(cfg.Storage.Local.Root)
		if err != nil {
			return nil, nil, err
		}
		caps := s.Capabilities()
		log.Info("storage ready",
			"backend", cfg.Storage.Backend, "layout", string(s.Mode()),
			"root", cfg.Storage.Local.Root,
			"deduplicated", caps.Deduplicated, "inspectable", caps.DirectFSInspect)
		// The tree layout names files by path, not by hash, so there is nothing for the scrub to
		// compare against and no blob store to hand it.
		return s, nil, nil
	}

	if cfg.Storage.Backend == "s3" {
		return openS3Storage(cfg, meta, log)
	}

	if cfg.Storage.Backend != "local" {
		return nil, nil, fmt.Errorf("storage backend %q is not supported; use \"local\" or \"s3\"", cfg.Storage.Backend)
	}

	ls, err := local.New(cfg.Storage.Local.Root)
	if err != nil {
		return nil, nil, err
	}
	// Prove a blob can be published with a single rename before serving traffic.
	// Splitting tmp and content across filesystems degrades that to a full copy,
	// which at scale is an outage rather than a slowdown.
	if err := ls.CheckSameFilesystem(); err != nil {
		return nil, nil, err
	}

	log.Info("storage ready",
		"backend", cfg.Storage.Backend, "layout", string(layout.ModeBlob),
		"root", cfg.Storage.Local.Root, "deduplicated", true, "inspectable", false)

	// Stage uploads inside the blob root: a container may have a read-only /tmp, and
	// staging on another filesystem would turn the publish into a full copy.
	adapter, err := blobstore.New(ls, &resolverAdapter{r: meta.Resolver()})
	if err != nil {
		return nil, nil, err
	}
	return adapter.WithSpoolDir(filepath.Join(cfg.Storage.Local.Root, "tmp")), ls, nil
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
	// This was a stub returning "no grant", which was the fail-closed answer while
	// acl_entries had no reader. It now resolves real grants: the user's direct
	// grants plus those of any group they belong to, accumulated up the ancestor
	// chain while inheritance allows.
	var (
		perms storepkg.Perm
		ok    bool
	)
	err := a.m.WithTx(context.Background(), func(ctx context.Context, tx storepkg.Tx) error {
		p, found, gerr := tx.EffectivePermsFor(ctx, actorID, nodeID)
		perms, ok = p, found
		return gerr
	})
	if err != nil {
		if errors.Is(err, storepkg.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, err
	}
	// The two packages declare the same bit layout; the conversion is explicit so a
	// future change to one is a compile-time reminder rather than a silent widening.
	return authz.Perm(perms), ok, nil
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

	cfg, err := config.Load(configPath())
	if err != nil {
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

	// Reconcile name_key with the current key function (T-12).
	//
	// Migration 0007 backfilled the column with SQL lower(), which cannot do NFC or full case
	// folding. For ASCII names the two agree, so drift is invisible until a name arrives with a
	// decomposed accent (macOS) or a foldable pair (ß/ss). A stale key makes the affected file
	// unreachable by the spelling the user types, so this converges the column rather than
	// leaving it to "self-heal on next write" — a file nobody renames never gets rewritten.
	rep, err := storepkg.RepairNameKeys(ctx, pool)
	if err != nil {
		return err
	}
	switch {
	case rep.Corrected > 0:
		log.Warn("name keys reconciled",
			"scanned", rep.Scanned, "corrected", rep.Corrected, "skipped", len(rep.Skipped))
	case rep.Scanned > 0:
		log.Info("name keys already consistent", "scanned", rep.Scanned)
	}
	// Skipped rows are reported individually: each is a real ambiguity that an operator has to
	// resolve by renaming one of the files, and burying them in a count would hide that.
	for _, s := range rep.Skipped {
		log.Warn("name key not reconciled", "detail", s)
	}

	// Re-derive blobs.size from the stored objects (INV-07).
	//
	// The column was written as 0 by BlobIDFor for as long as that function existed, so every
	// blob row from before the fix claims to be empty. The earlier note here said those rows
	// could not be recovered; that was wrong — the objects are content-addressed and still on
	// disk, so the hash names the object and the object knows its length.
	//
	// A missing object is reported rather than silently given a size: a blob row pointing at
	// content that is not there is an integrity problem, and papering over it would hide the one
	// thing the operator needs to see.
	if err := repairBlobSizes(ctx, cfg, pool, log); err != nil {
		return err
	}

	return nil
}

// repairBlobSizes opens the configured content store and corrects blobs.size.
//
// The store is opened here rather than inside the store package so the metadata layer never has
// to know which backend is configured. Failing to open it is not fatal to the migration: the
// schema is already applied, and refusing to exit would turn a storage misconfiguration into a
// blocked deploy. It is logged loudly instead.
func repairBlobSizes(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) error {
	blobs, err := openBlobStore(cfg)
	if err != nil {
		log.Warn("blob size backfill skipped", "error", err)
		return nil
	}

	rep, err := storepkg.RepairBlobSizes(ctx, pool, blobs.Stat)
	if err != nil {
		return err
	}
	switch {
	case rep.Fixed > 0:
		log.Warn("blob sizes reconciled",
			"scanned", rep.Scanned, "fixed", rep.Fixed, "recovered_bytes", rep.TotalFixedBytes,
			"missing", len(rep.Missing))
	case rep.Scanned > 0:
		log.Info("blob sizes consistent", "scanned", rep.Scanned)
	}
	for _, m := range rep.Missing {
		log.Warn("blob content not found; size left unchanged", "detail", m)
	}
	if rep.BackendError != nil {
		log.Warn("the content store reported errors during the backfill", "error", rep.BackendError)
	}
	return nil
}

// openBlobStore builds the content-addressed backend the backfill needs.
//
// The ordinary content store is a layout adapter whose Stat takes a *path*, because that is what
// the rest of the server asks for. The backfill has a content address, not a path, so it needs the
// layer below the adapter. Both backends are content-addressed, so both expose it — the tree
// layout is not, and is reported as unsupported rather than guessed at.
func openBlobStore(cfg config.Config) (blob.Store, error) {
	if cfg.Storage.Layout == config.LayoutTree {
		return nil, errors.New("the tree layout is not content-addressed, so blob sizes cannot be derived")
	}
	if cfg.Storage.Backend == "s3" {
		s3cfg := cfg.Storage.S3
		return s3blob.New(context.Background(), s3blob.Config{
			Endpoint:  s3cfg.Endpoint,
			Bucket:    s3cfg.Bucket,
			Region:    s3cfg.Region,
			AccessKey: s3cfg.AccessKey,
			SecretKey: s3cfg.SecretKey,
			Secure:    s3cfg.UseTLS(),
			Prefix:    s3cfg.Prefix,
		})
	}
	return local.New(cfg.Storage.Local.Root)
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

// breakglassCommand generates the credential file the local administrator uses.
//
// It is a subcommand rather than a documented Go snippet because the alternative is an
// operator writing code to recover access to their own system — exactly when they are
// least able to. The secret is read from stdin so it never appears in a shell history
// or in `ps` output.
func breakglassCommand(args []string) error {
	if len(args) == 0 || args[0] != "hash" {
		return errors.New("breakglass: missing sub-command (want `breakglass hash`)")
	}

	fmt.Fprintln(os.Stderr, "Reading the password from stdin. It is not echoed and not stored.")
	fmt.Fprintln(os.Stderr, "Write the output to the file named by auth.local_admin.password_argon2_file.")

	secret, err := readSecretLine(os.Stdin)
	if err != nil {
		return err
	}
	if len(secret) < 12 {
		// The one credential that bypasses the IdP deserves a floor. This is not a
		// strength policy — it is a refusal to accept "admin".
		return errors.New("breakglass: the password must be at least 12 characters")
	}

	h, err := password.Hash(secret, password.Default())
	if err != nil {
		return err
	}
	// Hex so the file is readable and can be compared against what an operator wrote
	// down; the hash is salted, so publishing one does not help an attacker.
	fmt.Println(hex.EncodeToString(h))
	return nil
}

// readSecretLine reads one line from r, rejecting an empty value.
//
// No terminal echoing control is attempted: this runs on whatever shell invoked it, and
// a half-working no-echo implementation is worse than a documented "the shell may echo
// what you type". The value is read from a pipe in every documented usage.
func readSecretLine(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", errors.New("breakglass: no password on stdin")
	}
	secret := strings.TrimRight(sc.Text(), "\r\n")
	if secret == "" {
		return "", errors.New("breakglass: empty password")
	}
	return secret, nil
}

// logVerifyReport announces the outcome of a chain verification.
//
// The success line used to be unconditional: a BROKEN chain still printed "audit chain verified"
// on stdout before the command failed on stderr. Anything that reads logs — a human at 3 a.m., or a
// log-scraping alert — sees the reassuring line and not the exit code. A verification command that
// announces success for the thing it rejects is worse than one that says nothing at all.
func logVerifyReport(log *slog.Logger, report auditverify.Report) {
	if report.OK() {
		log.Info("audit chain verified",
			"events", report.Events,
			"head", hex.EncodeToString(report.Head),
			"first_event", formatOptional(report.FirstEvent),
			"last_event", formatOptional(report.LastEvent))
		return
	}
	log.Error("audit chain BROKEN",
		"events", report.Events,
		"broken_at", report.BrokenAt,
		"error", fmt.Sprint(report.Err),
		"first_event", formatOptional(report.FirstEvent),
		"last_event", formatOptional(report.LastEvent))
}

// warnAboutUnusedConfig says out loud which configured key does nothing yet.
//
// server.usercontent_url is REQUIRED by verification — it must be an absolute URL that differs from
// public_url — and the server never reads it. Serving user content from a separate origin is not
// implemented, so the isolation the key implies does not exist. A required-but-inert key is the
// quiet kind of wrong: the deployment is forced to set it, the operator concludes the protection is
// on, and nothing says otherwise. Until origin separation exists, the gap is announced at startup
// rather than discovered during an audit.
func warnAboutUnusedConfig(log *slog.Logger, cfg config.Config) {
	for _, u := range cfg.UnusedKeys() {
		log.Warn("configuration has no effect", "key", u.Key, "why", u.Why)
	}
}

// verifyStart is the hash verification begins from.
//
// A zero mark means the log has never been pruned, and the chain starts at genesis. Passing the
// mark's nil HashHash in that case told the verifier to expect a nil prev_hash on the first event,
// which is never written — so a perfectly intact log reported "prev_hash does not match" at event 1.
func verifyStart(mark storepkg.PruneMark) []byte {
	if mark.ThroughID == 0 || len(mark.HeadHash) == 0 {
		return audit.GenesisHash
	}
	return mark.HeadHash
}
