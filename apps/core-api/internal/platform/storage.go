package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ErrObjectNotFound is Get's miss signal — a distinct error so the
// quote_send worker can tell "the render artifact is gone" (deterministic,
// refuse) from a transient storage failure (retry).
var ErrObjectNotFound = errors.New("platform: object not found")

// ObjectStorage is the object-storage seam for pipeline artifacts (ADR-003:
// Cloudflare R2 bucket quotepilot-dev). Only core-api ever holds storage
// credentials; ai-engine receives content, never keys. The interface is
// deliberately minimal: P1's rfq_ingest writes attachments, P5's quote_send
// reads the rendered PDF back. The real S3-compatible client lands with the
// R2 credentials (spec open item); until then main binds DirStorage so dev
// runs end-to-end without losing attachment bytes on restart.
type ObjectStorage interface {
	// Put stores body under key (e.g. "tenants/{org}/rfqs/{rfq}/{file}").
	// The caller builds tenant-safe keys; implementations must treat the key
	// as an opaque path and never interpolate credentials or org data.
	Put(ctx context.Context, key string, body []byte) error
	// Get returns the bytes stored under key; ErrObjectNotFound when the
	// key is absent. Implementations copy the buffer out — mutating the
	// returned slice must not corrupt the stored object.
	Get(ctx context.Context, key string) ([]byte, error)
}

// DirStorage is the dev binding for ObjectStorage: Put writes the object to
// root/key on local disk, creating parent directories. Bytes survive a
// restart — unlike FakeStorage — so the rfq_attachments rows they back stay
// resolvable until the real R2 client lands (spec open item). Keys are
// worker-generated, so the only guard is refusing ".." — a segment that
// would let a malformed key climb out of the root.
type DirStorage struct {
	root string
}

// NewDirStorage returns a store rooted at dir (created lazily on first Put).
//
//	store := platform.NewDirStorage(".dev-storage")
func NewDirStorage(root string) *DirStorage {
	return &DirStorage{root: root}
}

// Root reports the configured directory — wiring tests assert on it;
// workers never need it (they hold the Put/Get interface only).
func (d *DirStorage) Root() string { return d.root }

// Put writes body to root/key (0o600, parents 0o750 — tenant bytes stay
// owner-only). The slash-separated worker key maps through
// filepath.FromSlash so it stays portable.
func (d *DirStorage) Put(_ context.Context, key string, body []byte) error {
	if strings.Contains(key, "..") {
		return fmt.Errorf("platform: storage key %q must not contain '..'", key)
	}
	p := filepath.Join(d.root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return fmt.Errorf("platform: storage mkdir for %q: %w", key, err)
	}
	if err := os.WriteFile(p, body, 0o600); err != nil {
		return fmt.Errorf("platform: storage write %q: %w", key, err)
	}
	return nil
}

// Get reads root/key back; a missing file is ErrObjectNotFound — the same
// ".." guard applies so a malformed key cannot climb out of the root.
func (d *DirStorage) Get(_ context.Context, key string) ([]byte, error) {
	if strings.Contains(key, "..") {
		return nil, fmt.Errorf("platform: storage key %q must not contain '..'", key)
	}
	body, err := os.ReadFile(filepath.Join(d.root, filepath.FromSlash(key)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("platform: storage read %q: %w", key, err)
	}
	return body, nil
}

// FakeStorage is the dev/test implementation of ObjectStorage: it captures
// key + bytes in memory so worker tests can assert exactly what would have
// been uploaded, with no network. Contents vanish on restart — it must not
// be used where persistence matters.
type FakeStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
}

// NewFakeStorage returns an empty in-memory store.
//
//	store := platform.NewFakeStorage()
func NewFakeStorage() *FakeStorage {
	return &FakeStorage{objects: map[string][]byte{}}
}

// Put copies body into the in-memory map so a later mutation of the caller's
// slice cannot corrupt the captured object.
func (f *FakeStorage) Put(_ context.Context, key string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(body))
	copy(cp, body)
	f.objects[key] = cp
	return nil
}

// Get returns a copy of the stored object, ErrObjectNotFound when the key
// is absent — the interface contract a worker's miss-vs-transient
// classification depends on.
func (f *FakeStorage) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.objects[key]
	if !ok {
		return nil, ErrObjectNotFound
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	return cp, nil
}

// Keys returns the sorted set of stored keys — deterministic for assertions.
func (f *FakeStorage) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
