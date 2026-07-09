package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Verification contract:
// npm mode must support pnpm install-path reads for unscoped and scoped
// packages, rewrite dist.tarball URLs back to Toru, and reject unsupported
// mutation endpoints explicitly.

func TestNPMMetadataRequestReturnsRewrittenTarballs(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)

	cfgText := testMixedModeConfig(t, goPort, npmPort, upstream.URL)
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, "http://127.0.0.1:"+strconv.Itoa(goPort)+"/metrics", 10*time.Second)

	resp, body, err := getURL("http://127.0.0.1:" + strconv.Itoa(npmPort) + "/toru-fixture-pkg")
	if err != nil {
		t.Fatalf("request npm metadata: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%q, want 200", resp.StatusCode, body)
	}
	want := "http://127.0.0.1:" + strconv.Itoa(npmPort) + "/toru-fixture-pkg/-/toru-fixture-pkg-1.0.0.tgz"
	if !strings.Contains(body, want) {
		t.Fatalf("rewritten tarball URL missing: want substring %q in %q", want, body)
	}
}

func TestNPMScopedMetadataRequestReturnsRewrittenTarballs(t *testing.T) {
	for _, scopedPath := range []string{"/@toru/fixture-scoped", "/%40toru%2Ffixture-scoped", "/@toru%2ffixture-scoped"} {
		t.Run(scopedPath, func(t *testing.T) {
			goPort := freePort(t)
			npmPort := freePort(t)
			upstream := newFakeNPMRegistry(t)

			cfgText := testMixedModeConfig(t, goPort, npmPort, upstream.URL)
			proc := startToruProcess(t, cfgText)
			defer stopCmd(t, proc)

			waitForHTTP200(t, "http://127.0.0.1:"+strconv.Itoa(goPort)+"/metrics", 10*time.Second)

			resp, body, err := getURL("http://127.0.0.1:" + strconv.Itoa(npmPort) + scopedPath)
			if err != nil {
				t.Fatalf("request scoped npm metadata: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%q, want 200", resp.StatusCode, body)
			}
			want := "http://127.0.0.1:" + strconv.Itoa(npmPort) + "/%40toru%2Ffixture-scoped/-/fixture-scoped-1.0.0.tgz"
			if !strings.Contains(body, want) {
				t.Fatalf("rewritten scoped tarball URL missing: want substring %q in %q", want, body)
			}
		})
	}
}

func TestNPMCachePackageKeyAvoidsScopedCollisions(t *testing.T) {
	keyA := npmCachePackageKey("@a/b__c")
	keyB := npmCachePackageKey("@a__b/c")
	if keyA == keyB {
		t.Fatalf("cache keys must not collide: %q == %q", keyA, keyB)
	}
}

func TestUnsupportedNPMMutationEndpointRejected(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)

	cfgText := testMixedModeConfig(t, goPort, npmPort, upstream.URL)
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, "http://127.0.0.1:"+strconv.Itoa(goPort)+"/metrics", 10*time.Second)

	req, err := http.NewRequest(http.MethodPut, "http://127.0.0.1:"+strconv.Itoa(npmPort)+"/toru-fixture-pkg", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("npm mutation status=%d, want 405 or 501", resp.StatusCode)
	}
}
