package platform

import "context"

// MagicSend is one recorded fake send: recipient plus the full magic URL.
type MagicSend struct {
	To  string
	URL string
}

// FakeMailSender is the test double for MailSender: it records every call
// and never touches the network. It lives in the platform package (not a
// _test.go file) because internal/auth handler tests in PRs C1–C6 assert
// which (to, url) pairs went out — the uniform-response contract is proven
// by what the fake did NOT receive. Nothing wires it in production.
//
//	fake := &platform.FakeMailSender{}
//	handler.ServeHTTP(w, req)
//	assert.Empty(t, fake.Sends)
type FakeMailSender struct {
	Sends []MagicSend
}

var _ MailSender = (*FakeMailSender)(nil)

// SendMagicLink records the pair; it never fails.
//
//	fake := &platform.FakeMailSender{}
//	_ = fake.SendMagicLink(ctx, to, url)
//	assert.Len(t, fake.Sends, 1)
func (f *FakeMailSender) SendMagicLink(_ context.Context, to, url string) error {
	f.Sends = append(f.Sends, MagicSend{To: to, URL: url})
	return nil
}
