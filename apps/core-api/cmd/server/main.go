// Command server runs the QuotePilot core API: the six auth/session routes
// generated from the OpenAPI contract, the Resend inbound webhook and the
// River workers that own staged jobs (ADR-004). It holds only the qp_app
// DSN — schema changes go through cmd/migrate as qp_migrator, never here.
//
//	APP_ENV=local PORT=8080 POSTGRES_DSN=postgres://qp_app:... WEB_URL=https://app.example \
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
	"strings"
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
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/quotes"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/resend"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/webapi"
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
// not half-live. startJobs precedes buildInbox and buildEndpoints on
// purpose: the worker registry needs the one mailer, and both surfaces
// need the built client for their in-transaction enqueue closures
// (migration 0004, spec S1).
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
	riverClient, err := startJobs(ctx, db, store, mailer, newReceiver(&cfg), newSender(&cfg), newStorage(), newOutboundFrom(&cfg))
	if err != nil {
		return err
	}
	inboxServer, err := buildInbox(db, store, enqueueIngest(riverClient))
	if err != nil {
		return err
	}
	endpoints := buildEndpoints(&cfg, db, store, enqueueAuthMail(riverClient))
	// The strict surface's approve delegate needs the built River client
	// for the in-transaction quote_send enqueue (post-build assignment,
	// like ingest.Inserter above); the web read surface joins it on the
	// same generated mux.
	endpoints.Approver = quotes.NewEndpoints(db, riverClient)
	endpoints.Web = webapi.NewEndpoints(db, store, &cfg, time.Now)
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

// enqueueIngest adapts the River client's InsertTx to the webhook's enqueue
// seam: the rfq_ingest job lands inside the receipt's tenant transaction —
// one commit with the inbound_messages row (spec S1). Queue and uniqueness
// come from IngestArgs.InsertOpts; a fresh correlation id roots the chain.
// The param is the JobInserter slice (not *river.Client) so wiring tests
// can stand in a fake.
func enqueueIngest(inserter jobs.JobInserter) inbox.EnqueueIngestFunc {
	return func(ctx context.Context, tx pgx.Tx, org, correlationID, messageID uuid.UUID) error {
		_, err := inserter.InsertTx(ctx, tx, jobs.IngestArgs{
			OrganizationID:   org,
			CorrelationID:    correlationID,
			InboundMessageID: messageID,
		}, nil)
		if err != nil {
			return fmt.Errorf("server: enqueue rfq_ingest %s: %w", messageID, err)
		}
		return nil
	}
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

// newReceiver picks the Resend Receiving client for the rfq_ingest worker.
// Without RESEND_API_KEY the unconfigured stub is bound — the server still
// boots, and fetches fail loudly as permanent errors (the RFQ shell with
// failure_reason), never silently dropped (same fail-soft shape as the
// dev-mailer gate, not a silent off-switch).
func newReceiver(cfg *platform.Config) resend.Receiver {
	if cfg.ResendAPIKey != "" {
		return resend.NewReceivingClient(cfg.ResendAPIKey)
	}
	slog.Warn("server: RESEND_API_KEY unset — rfq_ingest binds the unconfigured receiver (fetches fail loudly)")
	return resend.UnconfiguredReceiver{}
}

// newSender picks the Resend Sending client for the quote_send worker —
// the same fail-soft contract as the receiver: without RESEND_API_KEY the
// unconfigured stub binds and sends fail loudly as permanent errors, so a
// staged send cancels instead of retrying a credential that cannot appear.
func newSender(cfg *platform.Config) resend.Sender {
	if cfg.ResendAPIKey != "" {
		return resend.NewSendingClient(cfg.ResendAPIKey)
	}
	slog.Warn("server: RESEND_API_KEY unset — quote_send binds the unconfigured sender (sends fail loudly)")
	return resend.UnconfiguredSender{}
}

// devDefaultOutboundFrom is the boot fallback when RESEND_API_KEY is set
// but QP_OUTBOUND_FROM is not: Resend's own always-verified sender, so dev
// keeps delivering without a verified domain. Production must set
// QP_OUTBOUND_FROM — the warn below is the alarm; sending From an
// unverified domain is a permanent 4xx that would cancel every send.
const devDefaultOutboundFrom = "onboarding@resend.dev"

// newOutboundFrom resolves the verified From: mailbox for quote_send. The
// fail-soft rule matches the receiver/sender stubs: warn loudly, never
// refuse boot — a missing QP_OUTBOUND_FROM degrades to the dev-safe
// default rather than killing the server, because refusing boot would make
// a missing cosmetic var a stricter gate than the API key itself (and a
// wrong-domain From already fails loudly as an audited permanent error).
func newOutboundFrom(cfg *platform.Config) string {
	if from := strings.TrimSpace(cfg.OutboundFrom); from != "" {
		return from
	}
	if cfg.ResendAPIKey != "" {
		slog.Warn("server: QP_OUTBOUND_FROM unset — quote_send From falls back to " + devDefaultOutboundFrom + " (dev-safe only; set a verified domain)")
	}
	return devDefaultOutboundFrom
}

// newStorage binds the object-storage seam. R2 credentials are a pending
// spec open item, so every environment gets the directory-backed dev store
// for now — bytes land under QP_STORAGE_DIR (default .dev-storage) and
// survive restarts, which keeps the rfq_attachments rows resolvable. The
// real S3-compatible client swaps in later without touching the worker.
func newStorage() platform.ObjectStorage {
	dir := os.Getenv("QP_STORAGE_DIR")
	if dir == "" {
		dir = ".dev-storage"
	}
	slog.Warn("server: object storage not configured (R2 creds pending) — dev directory storage", "dir", dir)
	return platform.NewDirStorage(dir)
}

// buildInbox mounts the Resend inbound webhook only when its Svix secret is
// configured. Empty skips the mount — a secretless webhook cannot verify
// anything, so the route must not exist; a malformed secret is a boot
// failure, because silently running without inbound mail is worse.
func buildInbox(db *tenantdb.DB, store *platform.Store, enqueue inbox.EnqueueIngestFunc) (http.Handler, error) {
	secret := os.Getenv("RESEND_WEBHOOK_SECRET")
	if secret == "" {
		slog.Warn("server: RESEND_WEBHOOK_SECRET unset — inbound webhook not mounted")
		return nil, nil
	}
	verifier, err := inbox.NewSvixVerifier(secret)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	return inbox.NewServer(db, store, verifier, enqueue, slog.Default()), nil
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
	// POST /quotes/{id}/approve — spec section 4 "Approval": the analyst's
	// central act. RequireRole narrows membership to the approval set; the
	// strict surface inside authSrv routes the path to the QuoteApprover
	// delegate. CheckOrigin guards the qp_org cookie transport: the
	// fallback ended "the custom header is the CSRF defense" — a same-site
	// sibling origin posts cookies freely and a browser POST always
	// carries Origin or Sec-Fetch-Site for CrossOriginProtection to judge.
	mux.Handle("POST /quotes/{id}/approve", platform.Correlation(
		platform.CheckOrigin(cfg)(
			platform.Authenticate(cfg)(
				platform.ResolveTenant(store)(
					platform.RequireRole(
						platform.RoleOwner, platform.RoleAdmin, platform.RolePricingAnalyst)(
						authSrv))))))
	// The web v1 read surface (core-api.yaml's web tag). The session pair
	// mounts Authenticate only — GET /v1/session resolves its own org hint
	// (OrgHints plants header + qp_org for the handler's fallback chain)
	// and stamps the resolved org onto qp_org itself; the org switch is
	// state-changing so CheckOrigin guards it. The two org-scoped reads
	// run ResolveTenantDefault — header, qp_org cookie, then the
	// first-membership default for safe methods — so a fresh session
	// (qp_access only, no qp_org yet) loads the inbox instead of 400ing.
	mux.Handle("GET /v1/session", platform.Correlation(
		platform.Authenticate(cfg)(webapi.OrgHints(authSrv))))
	mux.Handle("POST /v1/session/organization", platform.Correlation(
		platform.CheckOrigin(cfg)(platform.Authenticate(cfg)(authSrv))))
	mux.Handle("GET /v1/inbox/quotes", platform.Correlation(
		platform.Authenticate(cfg)(platform.ResolveTenantDefault(store)(authSrv))))
	mux.Handle("GET /v1/quotes/{quoteId}", platform.Correlation(
		platform.Authenticate(cfg)(platform.ResolveTenantDefault(store)(authSrv))))
	if inboxSrv != nil {
		mux.Handle("/webhooks/", platform.Correlation(platform.CheckOrigin(cfg)(inboxSrv)))
	}
	return mux
}

// startJobs builds the River client over the qp_app pool — the driver
// shares the request path's connections — with the worker registry: the
// spike's echo_tenant plus auth_mail_send, the durable magic-link delivery
// whose outbox the login transaction stages (migration 0004), and
// rfq_ingest, stage S1 of the quote pipeline (spec section 4). The reindexer
// is disabled (its fast schedule was a spike affordance; production never
// owns it).
func startJobs(ctx context.Context, db *tenantdb.DB, store *platform.Store, mailer platform.MailSender, recv resend.Receiver, sender resend.Sender, storage platform.ObjectStorage, outboundFrom string) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, jobs.NewEchoTenantWorker(db))
	river.AddWorker(workers, jobs.NewAuthMailWorker(store, mailer, time.Now))
	ingest := jobs.NewRfqIngestWorker(db, recv, storage)
	river.AddWorker(workers, ingest)
	price := jobs.NewRfqPriceWorker(db, time.Now)
	river.AddWorker(workers, price)
	river.AddWorker(workers, jobs.NewQuoteSendWorker(db, sender, storage, outboundFrom))
	client, err := river.NewClient(riverpgxv5.New(db.Pool()), &river.Config{
		// Only queues with registered workers are declared: rfq_ingest
		// chains rfq_extract into the `extract` queue, whose worker lands in
		// a later slice — an undeclared queue simply waits unworked instead
		// of the job failing with UnknownJobKindError. The `price` queue
		// hosts both S3a (rfq_fetch_rates, lands with the S3a worker) and
		// S3b (rfq_price, registered below) per spec §3's stage table.
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 4},
			jobs.QueueIngest:   {MaxWorkers: 4},
			jobs.QueuePrice:    {MaxWorkers: 4},
			jobs.QueueSend:     {MaxWorkers: 4},
		},
		Workers:           workers,
		Schema:            "river",
		Logger:            slog.Default(),
		ReindexerSchedule: river.NeverSchedule(),
		// rfq_ingest inserts rfq_extract (unregistered until the P2 worker
		// ships); without this River rejects the in-transaction enqueue.
		SkipUnknownJobCheck: true,
		// Without SoftStopTimeout the worker context inherits the Start ctx —
		// a signal would hard-cancel running jobs instantly instead of letting
		// Stop drain them inside the grace window.
		SoftStopTimeout: 15 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("server: river client: %w", err)
	}
	// The workers need the client for InsertTx; the client needs the worker
	// registry at construction — so the seam is assigned post-build,
	// pre-Start. rfq_price's enqueue is rfq_render, still unregistered:
	// SkipUnknownJobCheck (above) stays on until S4 lands.
	ingest.Inserter = client
	price.Inserter = client
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
