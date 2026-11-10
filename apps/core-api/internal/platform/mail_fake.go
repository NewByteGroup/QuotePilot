package platform

import (
	"context"
	"sync"
)

// MagicSend is one recorded fake send: recipient plus the full magic URL.
type MagicSend struct {
	To  string
	URL string
}

// FakeMailSender is the test double for MailSender: it records every call
// and never touches the network. It lives in the platform package (not a
// _test.go file) because internal/auth handler tests in PRs C1–C6 assert
// which (to, url) pairs went out — the uniform-response contract is proven
// by what the fake did NOT receive. Nothing wires it in production. Sends
// may run on detached goroutines (auth sends the magic link off the response
// path), so reads go through Sent and never touch the slice directly.
//
//	fake := &platform.FakeMailSender{}
//	handler.ServeHTTP(w, req)
//	assert.Empty(t, fake.Sent())
type FakeMailSender struct {
	mu    sync.Mutex
	sends []MagicSend
}

var _ MailSender = (*FakeMailSender)(nil)

// SendMagicLink records the pair; it never fails.
//
//	fake := &platform.FakeMailSender{}
//	_ = fake.SendMagicLink(ctx, to, url)
//	assert.Len(t, fake.Sent(), 1)
func (f *FakeMailSender) SendMagicLink(_ context.Context, to, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, MagicSend{To: to, URL: url})
	return nil
}

// Sent returns a copy of the recorded sends. The copy is safe to assert on
// while a detached send goroutine may still be recording the next one.
func (f *FakeMailSender) Sent() []MagicSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MagicSend(nil), f.sends...)
}
