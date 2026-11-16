package platform

import (
	"context"
	"log/slog"
)

// LogMailSender is the local-development MailSender, selected when the
// RESEND_API_KEY/AUTH_MAGIC_FROM pair is empty (config.go validates they
// travel together or not at all): a local stack has no mailbox to reach,
// so it "delivers" the link by logging it — the URL carries the login
// token on purpose, because clicking it from the log is how a developer
// signs in. It is a dev tool only: a deployment must configure the Resend
// pair, and this sender must never share a process with one.
type LogMailSender struct {
	logger *slog.Logger
}

var _ MailSender = (*LogMailSender)(nil)

// NewLogMailSender binds the dev sender to a logger.
//
//	sender := platform.NewLogMailSender(slog.Default())
func NewLogMailSender(logger *slog.Logger) *LogMailSender {
	return &LogMailSender{logger: logger}
}

// SendMagicLink logs the link at warn — dev noise that must be visible.
// The tokened URL in the line is this sender's entire purpose; the
// platform rule against logging tokens guards production diagnostics,
// and this type is never wired where production mail should flow.
func (s *LogMailSender) SendMagicLink(ctx context.Context, to, url string) error {
	s.logger.WarnContext(ctx, "platform: magic link (dev sender — no mailbox configured)",
		"to", to, "url", url)
	return nil
}
