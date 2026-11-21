package platform

import (
	"context"
	"sync"
)

// MagicSend is one recorded fake send: which template went out (the
// outbox row's purpose drove it), the recipient and the full tokened URL.
type MagicSend struct {
	Kind string
	To   string
	URL  string
}

// FakeMailSender is the test double for MailSender: it records every call
// and never touches the network. It lives in the platform package (not a
// _test.go file) because tests outside platform — the auth_mail_send
// worker's, today — assert which (to, url) pairs went out, and the
// uniform-response contract is proven by what the fake did NOT receive.
// Nothing wires it in production. Sends may run on worker goroutines, so
// reads go through Sent and never touch the slice directly.
//
//	fake := &platform.FakeMailSender{}
//	handler.ServeHTTP(w, req)
//	assert.Empty(t, fake.Sent())
type FakeMailSender struct {
	mu    sync.Mutex
	sends []MagicSend
}

var _ MailSender = (*FakeMailSender)(nil)

// SendMagicLink records the pair under the login kind; it never fails.
//
//	fake := &platform.FakeMailSender{}
//	_ = fake.SendMagicLink(ctx, to, url)
//	assert.Len(t, fake.Sent(), 1)
func (f *FakeMailSender) SendMagicLink(_ context.Context, to, url string) error {
	return f.record(MailPurposeLogin, to, url)
}

// SendPasswordReset records the pair under the reset kind — the template
// assertions the worker tests make read Sent()[i].Kind.
func (f *FakeMailSender) SendPasswordReset(_ context.Context, to, url string) error {
	return f.record(MailPurposePasswordReset, to, url)
}

// record is the one append both sends share.
func (f *FakeMailSender) record(kind, to, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, MagicSend{Kind: kind, To: to, URL: url})
	return nil
}

// Sent returns a copy of the recorded sends. The copy is safe to assert on
// while a worker goroutine may still be recording the next one.
func (f *FakeMailSender) Sent() []MagicSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MagicSend(nil), f.sends...)
}
