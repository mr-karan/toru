package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Verification contract:
// npm mode must fetch from upstream on first tarball request and then satisfy a
// repeat request from cache without another upstream tarball fetch.

func TestNPMTarballMissFetchesFromUpstreamAndCaches(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	var tarballHits atomic.Int32
	upstream := newCountingFakeNPMRegistry(t, &tarballHits)

	cfgText := testMixedModeConfig(t, goPort, npmPort, upstream.URL)
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	url := fmt.Sprintf("http://127.0.0.1:%d/toru-fixture-pkg/-/toru-fixture-pkg-1.0.0.tgz", npmPort)

	resp1, body1, err := getURL(url)
	if err != nil {
		t.Fatalf("first tarball request: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first tarball status=%d body=%q, want 200", resp1.StatusCode, body1)
	}
	if hits := tarballHits.Load(); hits != 1 {
		t.Fatalf("upstream tarball hits after first request = %d, want 1", hits)
	}

	resp2, body2, err := getURL(url)
	if err != nil {
		t.Fatalf("second tarball request: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second tarball status=%d body=%q, want 200", resp2.StatusCode, body2)
	}
	if hits := tarballHits.Load(); hits != 1 {
		t.Fatalf("upstream tarball hits after cached request = %d, want still 1", hits)
	}
}

func TestNPMTarballServedEvenWhenCacheWriteFails(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	var tarballHits atomic.Int32
	upstream := newCountingFakeNPMRegistry(t, &tarballHits)

	badCacheRoot := filepath.Join(t.TempDir(), "cache-file")
	if err := os.WriteFile(badCacheRoot, []byte("not-a-directory"), 0o644); err != nil {
		t.Fatalf("write bad cache root: %v", err)
	}

	cfgText := fmt.Sprintf(`[server]
address = ":%d"
log_level = "info"
fetch_timeout = "30s"

[[listeners]]
name = "go"
address = ":%d"
protocols = ["go"]

[[listeners]]
name = "npm"
address = ":%d"
protocols = ["npm"]

[cache]
enabled = true
type = "disk"
mutable_metadata_ttl = "0s"

[cache.disk]
path = %q

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = %q
metadata_ttl = "5m"
base_url = "http://127.0.0.1:%d"
`, goPort, goPort, npmPort, badCacheRoot, upstream.URL, npmPort)
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	url := fmt.Sprintf("http://127.0.0.1:%d/toru-fixture-pkg/-/toru-fixture-pkg-1.0.0.tgz", npmPort)
	resp, body, err := getURL(url)
	if err != nil {
		t.Fatalf("tarball request with bad cache root: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%q, want 200 despite cache write failure", resp.StatusCode, body)
	}
	if body != "fake-tgz-unscoped" {
		t.Fatalf("body=%q, want upstream tarball body", body)
	}
	if hits := tarballHits.Load(); hits != 1 {
		t.Fatalf("upstream tarball hits = %d, want 1", hits)
	}
}
