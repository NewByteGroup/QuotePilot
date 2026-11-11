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
// not half-live.
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
	endpoints, err := buildEndpoints(&cfg, db, store)
	if err != nil {
		return err
	}
	inboxServer, err := buildInbox(db)
	if err != nil {
		return err
	}
	riverClient, err := startJobs(ctx, db)
	if err != nil {
		return err
	}
	srv := newServer(&cfg, store, auth.NewServer(endpoints), inboxServer)
	return serve(ctx, srv, endpoints.WaitDetached, riverClient)
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
// the qp_app pool, the mailer and the spec section 8.1 limiter pair sharing
// one clock with the token TTLs. The returned Endpoints carries the
// detached-send counter serve drains on shutdown (auth.NewServer mounts
// the ctx-planting middleware pair itself).
func buildEndpoints(cfg *platform.Config, db *tenantdb.DB, store *platform.Store) (*auth.Endpoints, error) {
	mailer, err := newMailer(cfg)
	if err != nil {
		return nil, err
	}
	return auth.NewEndpoints(
		cfg, store, store.InStoreTx, db, mailer,
		platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, time.Now),
		platform.NewLimiter(platform.LoginEmailCapacity, platform.LimitWindow, time.Now),
		time.Now,
	), nil
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
func buildInbox(db *tenantdb.DB) (http.Handler, error) {
	secret := os.Getenv("RESEND_WEBHOOK_SECRET")
	if secret == "" {
		slog.Warn("server: RESEND_WEBHOOK_SECRET unset — inbound webhook not mounted")
		return nil, nil
	}
	verifier, err := inbox.NewSvixVerifier(secret)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	return inbox.NewServer(db, verifier, slog.Default()), nil
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
// shares the request path's connections — with the worker registry:
// echo_tenant only for now, the spike's proof. The reindexer is disabled
// (its fast schedule was a spike affordance; production never owns it).
func startJobs(ctx context.Context, db *tenantdb.DB) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, jobs.NewEchoTenantWorker(db))
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
func serve(ctx context.Context, srv httpServer, detached func(context.Context) bool, jobs jobStopper) error {
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
	return drain(srv, detached, jobs)
}

// drain owns the ordered shutdown inside one shared grace window: HTTP
// requests first (a finishing login may still have a detached send in
// flight), the detached sends themselves, then the workers.
func drain(srv httpServer, detached func(context.Context) bool, jobs jobStopper) error {
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	httpErr := srv.Shutdown(shutCtx)
	if !detached(shutCtx) {
		slog.Warn("server: detached sends still running at window end — links may be lost")
	}
	return errors.Join(httpErr, jobs.Stop(shutCtx))
}

// listenAddr reads PORT (the PaaS convention) with a dev default so a bare
// `go run ./cmd/server` still binds.
func listenAddr() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}
