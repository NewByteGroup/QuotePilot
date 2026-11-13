// Command server runs the QuotePilot core API: the six auth/session routes
// generated from the OpenAPI contract, the Resend inbound webhook and the
// River workers that own staged jobs (ADR-004). It holds only the qp_app
// DSN — schema changes go through cmd/migrate as qp_migrator, never here.
//
//	PORT=8080 POSTGRES_DSN=postgres://qp_app:... WEB_URL=https://app.example \
//	  AUTH_JWT_KEYS=k1=<32+ chars> RESEND_API_KEY=... AUTH_MAGIC_FROM=... \
//	  RESEND_WEBHOOK_SECRET=whsec_... go run ./cmd/server
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/inbox"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("server: fatal", "error", err)
		os.Exit(1)
	}
}

// run wires config, pool, surfaces and jobs, then serves until a signal
// asks for shutdown. Boot failures are fatal by construction: a server
// that cannot prove the qp_app role model or parse its own config must
// not half-live. startJobs precedes buildEndpoints on purpose: the worker
// registry needs the one mailer, and the endpoints need the built client
// for the in-transaction enqueue closure (migration 0004).
func run(ctx context.Context) error {
	cfg, err := platform.LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	db, err := tenantdb.Open(ctx, cfg.AppDSN)
	if err != nil {
		return fmt.Errorf("server: open qp_app pool: %w", err)
	}
	defer db.Close()
	store := platform.NewStore(db.Pool())
	mailer, err := newMailer(&cfg)
	if err != nil {
		return err
	}
	inboxServer, err := buildInbox(db, store)
	if err != nil {
		return err
	}
	riverClient, err := startJobs(ctx, db, store, mailer)
	if err != nil {
		return err
	}
	endpoints := buildEndpoints(&cfg, db, store, enqueueAuthMail(riverClient))
	srv := newServer(&cfg, store, auth.NewServer(endpoints), inboxServer)
	return serve(ctx, srv, riverClient)
}

// newServer assembles the bounded http.Server — every timeout is set so a
// stalled client cannot pin a connection (or a pool connection behind it)
// forever. WriteTimeout exceeds the drain window on purpose: the shutdown
// budget bounds shutdown, not a request's full write budget.
func newServer(cfg *platform.Config, store *platform.Store, authSrv, inboxSrv http.Handler) *http.Server {
	return &http.Server{
		Addr:              listenAddr(),
		Handler:           buildMux(cfg, store, authSrv, inboxSrv),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

// buildEndpoints composes the contract's six-route surface: the Store over
// the qp_app pool, the raw-tx transaction seam the staged send lands in,
// the enqueue closure and the spec section 8.1 limiter pair sharing one
// clock with the token TTLs. No MailSender is wired — the auth_mail_send
// worker owns delivery now (auth.NewServer mounts the ctx-planting
// middleware pair itself).
func buildEndpoints(cfg *platform.Config, db *tenantdb.DB, store *platform.Store, enqueue auth.EnqueueMailFunc) *auth.Endpoints {
	return auth.NewEndpoints(
		cfg, store, store.InStorePgxTx, db, enqueue,
		platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, time.Now),
		platform.NewLimiter(platform.LoginEmailCapacity, platform.LimitWindow, time.Now),
		time.Now,
	)
}

// enqueueAuthMail adapts the River client's InsertTx to the endpoints'
// enqueue seam: the auth_mail_send job lands inside the login transaction —
// one commit with the token row and the outbox row — carrying the outbox id
// only, so the tokened link never reaches river_job's qp_app-readable args
// (migration 0004).
func enqueueAuthMail(client *river.Client[pgx.Tx]) auth.EnqueueMailFunc {
	return func(ctx context.Context, tx pgx.Tx, outboxID uuid.UUID) error {
		_, err := client.InsertTx(ctx, tx, jobs.AuthMailArgs{OutboxID: outboxID}, nil)
		if err != nil {
			return fmt.Errorf("server: enqueue auth mail %s: %w", outboxID, err)
		}
		return nil
	}
}

// newMailer picks the sender: a configured Resend pair means real mail;
// both empty requires the explicit AUTH_DEV_MAILER opt-in for the
// LogMailSender — a deploy that forgets the pair must fail boot, never
// serve sign-in links into anyone's log tail (config.validate guarantees
// the pair never arrives half-set).
func newMailer(cfg *platform.Config) (platform.MailSender, error) {
	if cfg.ResendAPIKey != "" {
		return platform.NewResendMailSender(cfg.ResendAPIKey, cfg.MagicFrom), nil
	}
	if os.Getenv("AUTH_DEV_MAILER") == "true" {
		slog.Warn("server: AUTH_DEV_MAILER on — magic links go to the log (dev only)")
		return platform.NewLogMailSender(slog.Default()), nil
	}
	return nil, errors.New("server: no mail path — set RESEND_API_KEY and AUTH_MAGIC_FROM, or AUTH_DEV_MAILER=true for local dev")
}

// buildInbox mounts the Resend inbound webhook only when its Svix secret is
// configured. Empty skips the mount — a secretless webhook cannot verify
// anything, so the route must not exist; a malformed secret is a boot
// failure, because silently running without inbound mail is worse.
func buildInbox(db *tenantdb.DB, store *platform.Store) (http.Handler, error) {
	secret := os.Getenv("RESEND_WEBHOOK_SECRET")
	if secret == "" {
		slog.Warn("server: RESEND_WEBHOOK_SECRET unset — inbound webhook not mounted")
		return nil, nil
	}
	verifier, err := inbox.NewSvixVerifier(secret)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	return inbox.NewServer(db, store, verifier, slog.Default()), nil
}

// buildMux composes the surfaces with per-route middleware. Correlation is
// outermost on every mount so each request's logs share one id; CheckOrigin
// guards the state-changing mounts (/auth cookie flows, the webhook) —
// same-origin callers, the WEB_URL-trusted web app and non-browser clients
// pass, cross-site browser posts die before reaching a handler. /me wraps
// Authenticate only (the switcher list deliberately spans orgs);
// /me/organization adds ResolveTenant — the tenant-scoped read path. The
// /auth/* routes run without auth middleware: they are the credential
// exchange itself.
func buildMux(cfg *platform.Config, store *platform.Store, authSrv, inboxSrv http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/auth/", platform.Correlation(platform.CheckOrigin(cfg)(authSrv)))
	mux.Handle("GET /me", platform.Correlation(platform.Authenticate(cfg)(authSrv)))
	mux.Handle("GET /me/organization", platform.Correlation(
		platform.Authenticate(cfg)(platform.ResolveTenant(store)(authSrv))))
	if inboxSrv != nil {
		mux.Handle("/webhooks/", platform.Correlation(platform.CheckOrigin(cfg)(inboxSrv)))
	}
	return mux
}

// startJobs builds the River client over the qp_app pool — the driver
// shares the request path's connections — with the worker registry: the
// spike's echo_tenant plus auth_mail_send, the durable magic-link delivery
// whose outbox the login transaction stages (migration 0004). The reindexer
// is disabled (its fast schedule was a spike affordance; production never
// owns it).
func startJobs(ctx context.Context, db *tenantdb.DB, store *platform.Store, mailer platform.MailSender) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, jobs.NewEchoTenantWorker(db))
	river.AddWorker(workers, jobs.NewAuthMailWorker(store, mailer, time.Now))
	client, err := river.NewClient(riverpgxv5.New(db.Pool()), &river.Config{
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 4}},
		Workers:           workers,
		Schema:            "river",
		Logger:            slog.Default(),
		ReindexerSchedule: river.NeverSchedule(),
		// Without SoftStopTimeout the worker context inherits the Start ctx —
		// a signal would hard-cancel running jobs instantly instead of letting
		// Stop drain them inside the grace window.
		SoftStopTimeout: 15 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("server: river client: %w", err)
	}
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("server: start river: %w", err)
	}
	return client, nil
}

// httpServer is the slice of *http.Server serve drives — the seam tests
// inject a fake through.
type httpServer interface {
	ListenAndServe() error
	Shutdown(context.Context) error
}

// jobStopper is the slice of *river.Client serve drains.
type jobStopper interface {
	Stop(context.Context) error
	StopAndCancel(context.Context) error
}

// serve runs until the signal context closes or the server itself dies,
// then drains. A listener that dies on its own (bind failure) still owes
// the River client its stop — the deferred pool close must not race a
// running client.
func serve(ctx context.Context, srv httpServer, jobs jobStopper) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		_ = jobs.StopAndCancel(context.Background())
		return err
	case <-ctx.Done():
	}
	slog.Info("server: shutdown requested — draining")
	// A second signal during the drain should kill, not be swallowed by the
	// still-subscribed NotifyContext.
	signal.Reset(os.Interrupt, syscall.SIGTERM)
	return drain(srv, jobs)
}

// drain owns the ordered shutdown inside one shared grace window: HTTP
// requests first — a finishing login may still be committing its staged
// send — then River's Stop, under which workers keep delivering queued
// sends inside the client's SoftStopTimeout window. A send outliving the
// window still loses nothing: its outbox row stays for the next boot's
// worker to retry (migration 0004's at-least-once contract).
func drain(srv httpServer, jobs jobStopper) error {
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return errors.Join(srv.Shutdown(shutCtx), jobs.Stop(shutCtx))
}

// listenAddr reads PORT (the PaaS convention) with a dev default so a bare
// `go run ./cmd/server` still binds.
func listenAddr() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}
