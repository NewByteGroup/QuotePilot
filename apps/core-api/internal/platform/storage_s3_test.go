package platform

// Internal-package test file: the AWS SigV4 golden vector must drive the
// unexported signing helpers directly (service "iam" — S3Storage itself is
// pinned to "s3"), so this file sits inside package platform like the fx
// and webapi precedent tests.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestS3SigV4AWSGoldenVector runs AWS's published Signature Version 4 worked
// example — GET iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08
// with the docs' fixed key, secret, region and timestamp. Matching the
// documented signature proves canonicalization, key derivation and the
// HMAC chain all follow the spec, so R2 accepts what this adapter signs.
func TestS3SigV4AWSGoldenVector(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet,
		"https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("X-Amz-Date", "20150830T123600Z")
	empty := sha256.Sum256(nil)

	got := sigV4Signature(req,
		[]string{"content-type", "host", "x-amz-date"},
		hex.EncodeToString(empty[:]),
		"wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		"us-east-1", "iam", "20150830T123600Z")
	assert.Equal(t, "5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7", got)
}

// TestS3StoragePutGet exercises the adapter against a fake S3 HTTP server:
// path-style /{bucket}/{key} addressing, an AWS4-HMAC-SHA256 Authorization
// header and the payload hash in x-amz-content-sha256 — then Get reads the
// same bytes back through the same signing path.
func TestS3StoragePutGet(t *testing.T) {
	var putPath, auth, payloadHash string
	var stored []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putPath = r.URL.Path
			auth = r.Header.Get("Authorization")
			payloadHash = r.Header.Get("X-Amz-Content-Sha256")
			stored, _ = io.ReadAll(r.Body)
		}
		_, _ = w.Write(stored)
	}))
	defer ts.Close()
	s := NewS3Storage(ts.URL, "qp-bucket", "auto", "key", "secret", ts.Client())
	body := []byte("%PDF-1.4 fake")

	require.NoError(t, s.Put(context.Background(), "tenants/o1/rfqs/r1/a.pdf", body))
	assert.Equal(t, "/qp-bucket/tenants/o1/rfqs/r1/a.pdf", putPath)
	assert.True(t, strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=key/"),
		"Authorization must carry the SigV4 scheme, got %q", auth)
	sum := sha256.Sum256(body)
	assert.Equal(t, hex.EncodeToString(sum[:]), payloadHash)

	got, err := s.Get(context.Background(), "tenants/o1/rfqs/r1/a.pdf")
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

// TestS3StorageErrors proves the miss/transient split the workers depend
// on: 404 — or any non-2xx whose XML carries NoSuchKey — maps to
// ErrObjectNotFound, while a refused write surfaces the status and the
// server's error code in the wrapped error.
func TestS3StorageErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if strings.HasSuffix(r.URL.Path, "xml-miss") {
				w.WriteHeader(http.StatusBadRequest) // some S3 impls miss with 400
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
			_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code></Error>`))
	}))
	defer ts.Close()
	s := NewS3Storage(ts.URL, "b", "auto", "k", "s", nil)

	_, err := s.Get(context.Background(), "tenants/o/missing")
	assert.ErrorIs(t, err, ErrObjectNotFound)
	_, err = s.Get(context.Background(), "tenants/o/xml-miss")
	assert.ErrorIs(t, err, ErrObjectNotFound, "a NoSuchKey body is a miss even off-404")

	err = s.Put(context.Background(), "tenants/o/f", []byte("x"))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrObjectNotFound)
	assert.Contains(t, err.Error(), "403")
	assert.Contains(t, err.Error(), "AccessDenied")
}

// TestS3StorageRejectsTraversal applies the same ".." refusal as
// DirStorage — a malformed worker key must never reach a signed request,
// on either the write or the read path.
func TestS3StorageRejectsTraversal(t *testing.T) {
	s := NewS3Storage("https://x.example", "b", "auto", "k", "s", nil)
	err := s.Put(context.Background(), "tenants/../escape", []byte("x"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "..")

	_, err = s.Get(context.Background(), "../key")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrObjectNotFound, "traversal is a guard refusal, not a miss")
}
