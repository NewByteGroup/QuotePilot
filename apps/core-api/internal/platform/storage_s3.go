package platform

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// s3SigV4Service is the fixed SigV4 service name — R2, MinIO and LocalStack
// all authenticate under "s3"; the region distinguishes them ("auto" for
// R2).
const s3SigV4Service = "s3"

// s3MaxErrorSnippet caps how much of an S3 error body lands in the returned
// error — S3's <Error><Code> sits at the head, so the cap names the failure
// without dumping an arbitrary server-controlled payload into logs.
const s3MaxErrorSnippet = 256

// S3Storage is the production binding for ObjectStorage: a single-bucket
// S3-compatible client (Cloudflare R2 per ADR-003; the same path-style
// addressing also works against MinIO and LocalStack for local parity).
// It speaks raw Signature Version 4 over net/http instead of pulling an
// SDK — ADR-002's allow-list admits no AWS dependency, and the signing
// math is deterministic stdlib crypto, golden-tested against AWS's own
// published vector. The process holds the only copy of the secret; keys
// stay opaque worker-generated paths ("tenants/{org}/rfqs/{rfq}/{file}")
// and the same ".." refusal as DirStorage keeps a malformed key from ever
// reaching a signed request.
type S3Storage struct {
	endpoint        string
	bucket          string
	region          string
	accessKeyID     string
	secretAccessKey string
	hc              *http.Client
}

// NewS3Storage returns a client for endpoint (scheme://host — e.g.
// https://<account>.r2.cloudflarestorage.com) bound to bucket. region is
// "auto" for R2; hc may be nil for a sane 30s-timeout default.
//
//	store := platform.NewS3Storage(os.Getenv("QP_STORAGE_S3_ENDPOINT"),
//		"quotepilot-dev", "auto", keyID, secret, nil)
func NewS3Storage(endpoint, bucket, region, accessKeyID, secretAccessKey string, hc *http.Client) *S3Storage {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &S3Storage{
		endpoint:        strings.TrimRight(endpoint, "/"),
		bucket:          bucket,
		region:          region,
		accessKeyID:     accessKeyID,
		secretAccessKey: secretAccessKey,
		hc:              hc,
	}
}

// Endpoint reports the configured endpoint — wiring tests assert on it;
// workers never need it (they hold the Put/Get interface only).
func (s *S3Storage) Endpoint() string { return s.endpoint }

// Bucket reports the configured bucket — wiring tests assert on it;
// workers never need it (they hold the Put/Get interface only).
func (s *S3Storage) Bucket() string { return s.bucket }

// Region reports the signing region — wiring tests assert the R2 "auto"
// default; workers never need it (they hold the Put/Get interface only).
func (s *S3Storage) Region() string { return s.region }

// Put uploads body to {endpoint}/{bucket}/{key} as one SigV4-signed PUT.
// The payload sha256 rides in x-amz-content-sha256 — R2 verifies it, so a
// body corrupted in flight fails loudly there instead of persisting. Any
// non-2xx is an error carrying the status and a capped body snippet.
func (s *S3Storage) Put(ctx context.Context, key string, body []byte) error {
	if strings.Contains(key, "..") {
		return fmt.Errorf("platform: storage key %q must not contain '..'", key)
	}
	sum := sha256.Sum256(body)
	req, err := s.newRequest(ctx, http.MethodPut, key, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	s.sign(req, hex.EncodeToString(sum[:]), time.Now())
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("platform: s3 put %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("platform: s3 put %q: status %d: %s", key, resp.StatusCode, errorSnippet(resp))
	}
	_, _ = io.Copy(io.Discard, resp.Body) // drain for keep-alive reuse
	return nil
}

// Get downloads {endpoint}/{bucket}/{key} with a signed GET. 404 — or a
// non-2xx whose XML body carries NoSuchKey — is ErrObjectNotFound, the
// miss signal the quote_send worker's refuse-vs-retry classification
// depends on; every other non-2xx is a transient-shaped error.
func (s *S3Storage) Get(ctx context.Context, key string) ([]byte, error) {
	if strings.Contains(key, "..") {
		return nil, fmt.Errorf("platform: storage key %q must not contain '..'", key)
	}
	req, err := s.newRequest(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	empty := sha256.Sum256(nil)
	s.sign(req, hex.EncodeToString(empty[:]), time.Now())
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("platform: s3 get %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, ErrObjectNotFound
	}
	if resp.StatusCode/100 != 2 {
		snip := errorSnippet(resp)
		if strings.Contains(snip, "NoSuchKey") {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("platform: s3 get %q: status %d: %s", key, resp.StatusCode, snip)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("platform: s3 get %q read body: %w", key, err)
	}
	return body, nil
}

// newRequest builds the path-style object URL — endpoint may carry a path
// prefix (LocalStack), so the bucket and key append to whatever path is
// already there rather than replacing it.
func (s *S3Storage) newRequest(ctx context.Context, method, key string, body io.Reader) (*http.Request, error) {
	u, err := url.Parse(s.endpoint)
	if err != nil {
		return nil, fmt.Errorf("platform: s3 endpoint %q unparseable: %w", s.endpoint, err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + s.bucket + "/" + key
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("platform: s3 %s %q request: %w", method, key, err)
	}
	return req, nil
}

// sign stamps req with x-amz-date, x-amz-content-sha256 and the SigV4
// Authorization for service "s3" at instant t — every byte R2
// authenticates is a byte this adapter chose.
func (s *S3Storage) sign(req *http.Request, payloadHash string, t time.Time) {
	amzDate := t.UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	sig := sigV4Signature(req, signedHeaders, payloadHash,
		s.secretAccessKey, s.region, s3SigV4Service, amzDate)
	scope := amzDate[:8] + "/" + s.region + "/" + s3SigV4Service + "/aws4_request"
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 "+
		"Credential="+s.accessKeyID+"/"+scope+
		", SignedHeaders="+strings.Join(signedHeaders, ";")+
		", Signature="+sig)
}

// sigV4Signature runs the deterministic tail of SigV4 — canonical request
// → string-to-sign → derived key → hex HMAC — so the AWS documentation
// vector (service iam) can exercise the exact code "s3" requests ride on.
func sigV4Signature(req *http.Request, signedHeaders []string, payloadHash,
	secret, region, service, amzDate string) string {
	scope := amzDate[:8] + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(canonicalRequest(req, signedHeaders, payloadHash)))
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope,
		hex.EncodeToString(sum[:])}, "\n")
	return hex.EncodeToString(
		hmacSHA256(deriveSigningKey(secret, amzDate[:8], region, service), toSign))
}

// deriveSigningKey folds the secret through date→region→service→aws4_request
// — a leaked signature therefore scopes itself to one day, one region, one
// service; the raw secret never leaves this HMAC chain.
func deriveSigningKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// canonicalRequest flattens req into the SigV4 canonical form: method,
// per-segment-encoded path, sorted-encoded query, each signed header as
// "name:value\n", the signed-header list, and the payload hash. req must
// already carry every header named in signedHeaders.
func canonicalRequest(req *http.Request, signedHeaders []string, payloadHash string) string {
	var b strings.Builder
	b.WriteString(req.Method)
	b.WriteByte('\n')
	b.WriteString(canonicalURI(req.URL.Path))
	b.WriteByte('\n')
	b.WriteString(canonicalQuery(req.URL.Query()))
	b.WriteByte('\n')
	for _, name := range signedHeaders {
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(canonicalHeaderValue(req, name))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(strings.Join(signedHeaders, ";"))
	b.WriteByte('\n')
	b.WriteString(payloadHash)
	return b.String()
}

// canonicalURI re-encodes each path segment per RFC 3986 (unreserved bytes
// pass through, everything else %XX) while preserving the "/" separators —
// the exact form the signature commits to.
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		segs[i] = sigV4Encode(seg)
	}
	return strings.Join(segs, "/")
}

// canonicalQuery sorts and encodes query pairs the way SigV4 demands; the
// storage adapter itself sends none, but the AWS iam vector does — one
// code path covers both.
func canonicalQuery(values url.Values) string {
	pairs := make([]string, 0, len(values))
	for k, vs := range values {
		for _, v := range vs {
			pairs = append(pairs, sigV4Encode(k)+"="+sigV4Encode(v))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// canonicalHeaderValue folds one signed header to its SigV4 form: host
// resolves from the request line (Go carries it outside Header), other
// names read from Header, and whitespace runs collapse to single spaces.
func canonicalHeaderValue(req *http.Request, name string) string {
	var v string
	if name == "host" {
		v = req.Host
		if v == "" {
			v = req.URL.Host
		}
	} else {
		v = req.Header.Get(name)
	}
	return strings.Join(strings.Fields(v), " ")
}

// sigV4Encode percent-encodes every byte outside the RFC 3986 unreserved
// set — url.QueryEscape would emit '+' for space, which S3 rejects in
// signatures.
func sigV4Encode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if isUnreserved(s[i]) {
			b.WriteByte(s[i])
		} else {
			fmt.Fprintf(&b, "%%%02X", s[i])
		}
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' ||
		c == '-' || c == '_' || c == '.' || c == '~'
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(msg))
	return h.Sum(nil)
}

// errorSnippet drains up to s3MaxErrorSnippet bytes of a failed response —
// then drains the tail so the connection stays reusable for keep-alive.
func errorSnippet(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, s3MaxErrorSnippet))
	_, _ = io.Copy(io.Discard, resp.Body)
	return strings.TrimSpace(string(b))
}
