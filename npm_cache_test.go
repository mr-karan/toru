package main

import (
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

type fakeNPMCacheStore struct {
	mu      sync.Mutex
	bodies  map[string][]byte
	modTime map[string]time.Time
	now     func() time.Time
}

type failingNPMCacheStore struct {
	err error
}

func newFakeNPMCacheStore() *fakeNPMCacheStore {
	return &fakeNPMCacheStore{
		bodies:  map[string][]byte{},
		modTime: map[string]time.Time{},
		now:     time.Now,
	}
}

func (s *fakeNPMCacheStore) Get(key string) ([]byte, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.bodies[key]
	if !ok {
		return nil, time.Time{}, fs.ErrNotExist
	}
	copyBody := append([]byte(nil), body...)
	return copyBody, s.modTime[key], nil
}

func (s *fakeNPMCacheStore) Put(key string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies[key] = append([]byte(nil), body...)
	s.modTime[key] = s.now()
	return nil
}

func (s *fakeNPMCacheStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bodies, key)
	delete(s.modTime, key)
	return nil
}

func (s *failingNPMCacheStore) Get(string) ([]byte, time.Time, error) {
	return nil, time.Time{}, s.err
}

func (s *failingNPMCacheStore) Put(string, []byte) error {
	return s.err
}

func (s *failingNPMCacheStore) Delete(string) error {
	return s.err
}

func newTestNPMHandler(t *testing.T, upstream string, store npmCacheStore) *npmHandler {
	t.Helper()
	cfg := &Config{}
	cfg.Server.FetchTimeout = 5 * time.Second
	cfg.Cache.Enabled = true
	cfg.Cache.Type = "s3"
	cfg.Protocols.NPM.Enabled = true
	cfg.Protocols.NPM.Upstream = upstream
	cfg.Protocols.NPM.BaseURL = "http://example.com"
	cfg.Protocols.NPM.MetadataTTL = time.Hour
	return &npmHandler{
		cfg:            cfg,
		logger:         slog.Default(),
		httpClient:     &http.Client{Timeout: 5 * time.Second},
		authenticators: map[string]Authenticator{},
		npmCache:       store,
		now:            time.Now,
	}
}

func TestNewNPMHandlerUsesDiskCacheStoreCompatibility(t *testing.T) {
	cacheRoot := t.TempDir()
	cfg := &Config{}
	cfg.Server.FetchTimeout = 5 * time.Second
	cfg.Cache.Enabled = true
	cfg.Cache.Type = "disk"
	cfg.Cache.Disk.Path = cacheRoot
	cfg.Protocols.NPM.Enabled = true
	cfg.Protocols.NPM.Upstream = "https://registry.npmjs.org"
	cfg.Protocols.NPM.BaseURL = "http://example.com"
	cfg.Protocols.NPM.MetadataTTL = time.Hour

	handler, err := newNPMHandler(cfg, slog.Default(), map[string]Authenticator{})
	if err != nil {
		t.Fatalf("newNPMHandler: %v", err)
	}
	h := handler.(*npmHandler)
	if _, ok := h.npmCache.(*diskNPMCacheStore); !ok {
		t.Fatalf("npm cache store = %T, want *diskNPMCacheStore", h.npmCache)
	}
	wrote, err := h.writeMetadataCache("toru-fixture-pkg", []byte(`{"name":"toru-fixture-pkg"}`), `"etag-1"`)
	if err != nil {
		t.Fatalf("writeMetadataCache: %v", err)
	}
	if !wrote {
		t.Fatalf("writeMetadataCache wrote=false, want true")
	}
	for _, path := range []string{
		filepath.Join(cacheRoot, "npm-meta", "toru-fixture-pkg.json"),
		filepath.Join(cacheRoot, "npm-meta", "toru-fixture-pkg.etag"),
		filepath.Join(cacheRoot, "npm-meta", "toru-fixture-pkg.expiry"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected legacy disk cache path %q: %v", path, err)
		}
	}
}

func TestNewNPMHandlerUsesS3CacheStoreWhenConfigured(t *testing.T) {
	cfg := &Config{}
	cfg.Server.FetchTimeout = 5 * time.Second
	cfg.Cache.Enabled = true
	cfg.Cache.Type = "s3"
	cfg.Cache.S3.Region = "us-east-1"
	cfg.Cache.S3.Bucket = "toru-test"
	cfg.Cache.S3.AccessKey = "test"
	cfg.Cache.S3.SecretKey = "test"
	cfg.Protocols.NPM.Enabled = true
	cfg.Protocols.NPM.Upstream = "https://registry.npmjs.org"
	cfg.Protocols.NPM.BaseURL = "http://example.com"
	cfg.Protocols.NPM.MetadataTTL = time.Hour

	handler, err := newNPMHandler(cfg, slog.Default(), map[string]Authenticator{})
	if err != nil {
		t.Fatalf("newNPMHandler: %v", err)
	}
	h := handler.(*npmHandler)
	if _, ok := h.npmCache.(*s3NPMCacheStore); !ok {
		t.Fatalf("npm cache store = %T, want *s3NPMCacheStore", h.npmCache)
	}
}

func TestNPMMetadataCacheReadErrorsRecordCacheError(t *testing.T) {
	before := metrics.GetOrCreateCounter(`toru_cache_errors_by_protocol_total{protocol="npm",class="metadata"}`).Get()
	h := newTestNPMHandler(t, "https://registry.npmjs.org", &failingNPMCacheStore{err: fmt.Errorf("boom")})
	if _, ok := h.readMetadataCache("toru-fixture-pkg"); ok {
		t.Fatalf("readMetadataCache ok=true, want false on cache read error")
	}
	after := metrics.GetOrCreateCounter(`toru_cache_errors_by_protocol_total{protocol="npm",class="metadata"}`).Get()
	if after != before+1 {
		t.Fatalf("metadata cache error counter delta = %v, want 1", after-before)
	}
}

func TestNPMArtifactCacheReadErrorsRecordCacheError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pkg/-/pkg-1.0.0.tgz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("tarball-body"))
	}))
	defer upstream.Close()

	before := metrics.GetOrCreateCounter(`toru_cache_errors_by_protocol_total{protocol="npm",class="artifact"}`).Get()
	h := newTestNPMHandler(t, upstream.URL, &failingNPMCacheStore{err: fmt.Errorf("boom")})
	req := httptest.NewRequest(http.MethodGet, "http://toru.test/pkg/-/pkg-1.0.0.tgz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q, want 200", rr.Code, rr.Body.String())
	}
	after := metrics.GetOrCreateCounter(`toru_cache_errors_by_protocol_total{protocol="npm",class="artifact"}`).Get()
	if after < before+1 {
		t.Fatalf("artifact cache error counter delta = %v, want at least 1", after-before)
	}
}

func TestNPMMetadataCacheSupportsS3Store(t *testing.T) {
	store := newFakeNPMCacheStore()
	h := newTestNPMHandler(t, "https://registry.npmjs.org", store)

	wrote, err := h.writeMetadataCache("toru-fixture-pkg", []byte(`{"name":"toru-fixture-pkg"}`), `"etag-1"`)
	if err != nil {
		t.Fatalf("writeMetadataCache: %v", err)
	}
	if !wrote {
		t.Fatalf("writeMetadataCache wrote=false, want true")
	}
	entry, ok := h.readMetadataCache("toru-fixture-pkg")
	if !ok {
		t.Fatalf("readMetadataCache ok=false, want true")
	}
	if string(entry.Body) != `{"name":"toru-fixture-pkg"}` {
		t.Fatalf("entry body = %q", string(entry.Body))
	}
	if entry.ETag != `"etag-1"` {
		t.Fatalf("entry etag = %q, want %q", entry.ETag, `"etag-1"`)
	}
	if !entry.Fresh {
		t.Fatalf("entry should be fresh")
	}
}

func TestNPMTarballCacheUsesS3Store(t *testing.T) {
	var tarballHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pkg/-/pkg-1.0.0.tgz" {
			http.NotFound(w, r)
			return
		}
		tarballHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("tarball-body"))
	}))
	defer upstream.Close()

	h := newTestNPMHandler(t, upstream.URL, newFakeNPMCacheStore())

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://toru.test/pkg/-/pkg-1.0.0.tgz", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%q, want 200", i+1, rr.Code, rr.Body.String())
		}
		if rr.Body.String() != "tarball-body" {
			t.Fatalf("request %d body=%q, want tarball-body", i+1, rr.Body.String())
		}
	}
	if hits := tarballHits.Load(); hits != 1 {
		t.Fatalf("upstream tarball hits=%d, want 1", hits)
	}
}

func TestNPMRewriteTarballCacheUsesS3Store(t *testing.T) {
	archiveHits := &atomic.Int32{}
	gitlab := newFakeGitLabRegistry(t, map[string]fakeGitLabRepo{
		"commons/foo": {
			Tags:        []string{"v1.0.0"},
			ArchiveHits: archiveHits,
			Archives: map[string]map[string]string{
				"v1.0.0": {
					"package.json": `{"name":"@example-commons/foo","version":"1.0.0"}`,
					"index.js":     `module.exports = 42`,
				},
			},
		},
	})
	defer gitlab.Close()

	h := newTestNPMHandler(t, "https://registry.npmjs.org", newFakeNPMCacheStore())
	h.cfg.Protocols.NPM.RewriteRules = []NPMRewriteRule{{
		Scope:       "@example-commons",
		TargetHost:  gitlab.Host(),
		TargetGroup: "commons",
		AuthModule:  "static",
	}}
	h.authenticators["static"] = &StaticTokenAuthenticator{Token: "secret"}

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://toru.test/%%40example-commons%%2Ffoo/-/foo-1.0.0.tgz"), nil)
		req.SetBasicAuth("static", "secret")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%q, want 200", i+1, rr.Code, rr.Body.String())
		}
	}
	if hits := archiveHits.Load(); hits != 1 {
		t.Fatalf("gitlab archive hits=%d, want 1", hits)
	}
}
