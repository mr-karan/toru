package main

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/goproxy/goproxy"
)

func TestMutableMetadataBypassesPersistentCacheEndToEnd(t *testing.T) {
	t.Parallel()

	fetcher := &stubFetcher{
		listResults: [][]string{{"v0.1.0"}},
		listErrors:  []error{nil, fs.ErrNotExist},
	}
	cacher := newMetadataCacher(newMemoryCacher(), 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy := &goproxy.Goproxy{Fetcher: fetcher, Cacher: cacher}
	server := httptest.NewServer(proxy)
	defer server.Close()

	body1 := mustHTTPGetBody(t, server.URL+"/example.com/team/workflows/@v/list")
	resp, err := http.Get(server.URL + "/example.com/team/workflows/@v/list")
	if err != nil {
		t.Fatalf("second GET failed: %v", err)
	}
	defer resp.Body.Close()

	if string(body1) != "v0.1.0" {
		t.Fatalf("first /@v/list body = %q, want %q", string(body1), "v0.1.0")
	}
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("second /@v/list status = %d body=%q, want 404 without stale cache fallback", resp.StatusCode, string(body))
	}
	if fetcher.listCalls != 2 {
		t.Fatalf("expected fetcher List to be called twice, got %d", fetcher.listCalls)
	}
}

func TestImmutableModuleInfoIsCachedEndToEnd(t *testing.T) {
	t.Parallel()

	fetcher := &stubFetcher{
		downloads: map[string]downloadResult{
			"example.com/team/workflows@v1.2.3": {
				info: []byte(`{"Version":"v1.2.3","Time":"2026-04-16T12:00:00Z"}`),
				mod:  []byte("module example.com/team/workflows\n"),
				zip:  []byte("zip-content"),
			},
		},
	}
	cacher := newMetadataCacher(newMemoryCacher(), 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy := &goproxy.Goproxy{Fetcher: fetcher, Cacher: cacher}
	server := httptest.NewServer(proxy)
	defer server.Close()

	body1 := mustHTTPGetBody(t, server.URL+"/example.com/team/workflows/@v/v1.2.3.info")
	body2 := mustHTTPGetBody(t, server.URL+"/example.com/team/workflows/@v/v1.2.3.info")

	if !bytes.Equal(body1, body2) {
		t.Fatalf("cached .info response mismatch: first=%q second=%q", string(body1), string(body2))
	}
	if fetcher.downloadCalls != 1 {
		t.Fatalf("expected fetcher Download to be called once, got %d", fetcher.downloadCalls)
	}
	if fetcher.listCalls != 0 {
		t.Fatalf("expected fetcher List to remain unused, got %d calls", fetcher.listCalls)
	}
}

func mustHTTPGetBody(t *testing.T, url string) []byte {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s failed: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s returned status %d: %s", url, resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	return body
}

type stubFetcher struct {
	mu            sync.Mutex
	listResults   [][]string
	listErrors    []error
	listCalls     int
	downloads     map[string]downloadResult
	downloadCalls int
}

func (sf *stubFetcher) Query(context.Context, string, string) (string, time.Time, error) {
	return "", time.Time{}, nil
}

func (sf *stubFetcher) List(context.Context, string) ([]string, error) {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	idx := sf.listCalls
	sf.listCalls++
	if idx < len(sf.listErrors) && sf.listErrors[idx] != nil {
		return nil, sf.listErrors[idx]
	}
	if len(sf.listResults) == 0 {
		return nil, nil
	}
	if idx >= len(sf.listResults) {
		idx = len(sf.listResults) - 1
	}
	return append([]string(nil), sf.listResults[idx]...), nil
}

func (sf *stubFetcher) Download(_ context.Context, path, version string) (io.ReadSeekCloser, io.ReadSeekCloser, io.ReadSeekCloser, error) {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	sf.downloadCalls++
	content := sf.downloads[path+"@"+version]
	return newStaticReadSeekCloser(content.info), newStaticReadSeekCloser(content.mod), newStaticReadSeekCloser(content.zip), nil
}

type downloadResult struct {
	info []byte
	mod  []byte
	zip  []byte
}

type memoryCacher struct {
	mu      sync.Mutex
	entries map[string]memoryCacheEntry
}

type memoryCacheEntry struct {
	content      []byte
	lastModified time.Time
}

func newMemoryCacher() *memoryCacher {
	return &memoryCacher{entries: make(map[string]memoryCacheEntry)}
}

func (mc *memoryCacher) Get(_ context.Context, name string) (io.ReadCloser, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	entry, ok := mc.entries[name]
	if !ok {
		return nil, fs.ErrNotExist
	}

	return &memoryReadCloser{Reader: bytes.NewReader(entry.content), lastModified: entry.lastModified}, nil
}

func (mc *memoryCacher) Put(_ context.Context, name string, content io.ReadSeeker) error {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return err
	}
	body, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	mc.entries[name] = memoryCacheEntry{content: body, lastModified: time.Now()}
	return nil
}

type memoryReadCloser struct {
	*bytes.Reader
	lastModified time.Time
}

func (mrc *memoryReadCloser) Close() error {
	return nil
}

func (mrc *memoryReadCloser) LastModified() time.Time {
	return mrc.lastModified
}

type staticReadSeekCloser struct {
	*bytes.Reader
}

func newStaticReadSeekCloser(content []byte) io.ReadSeekCloser {
	return &staticReadSeekCloser{Reader: bytes.NewReader(content)}
}

func (src *staticReadSeekCloser) Close() error {
	return nil
}
