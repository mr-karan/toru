package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Verification contract:
// Mixed-mode Toru must be provable through observable behavior in one process:
// separate Go and npm listeners, metrics still available, and npm traffic not
// handled by the legacy Go-only path.

func TestMixedModeRuntimeExposesMetricsAndNPMListener(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)

	cfgText := testMixedModeConfig(t, goPort, npmPort, upstream.URL)
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	resp, body, err := getURL(fmt.Sprintf("http://127.0.0.1:%d/toru-fixture-pkg", npmPort))
	if err != nil {
		t.Fatalf("expected npm listener to accept metadata request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("npm metadata status = %d body=%q, want 200", resp.StatusCode, body)
	}
	if !strings.Contains(body, fmt.Sprintf("http://127.0.0.1:%d/toru-fixture-pkg/-/toru-fixture-pkg-1.0.0.tgz", npmPort)) {
		t.Fatalf("npm metadata must rewrite tarball URL back to Toru, body=%q", body)
	}
}

func testMixedModeConfig(t *testing.T, goPort, npmPort int, upstreamURL string) string {
	t.Helper()
	return fmt.Sprintf(`[server]
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
`, goPort, goPort, npmPort, filepath.Join(t.TempDir(), "cache"), upstreamURL, npmPort)
}

func newFakeNPMRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	return newCountingFakeNPMRegistry(t, nil)
}

func newCountingFakeNPMRegistry(t *testing.T, tarballHits *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/toru-fixture-pkg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"name":"toru-fixture-pkg",
			"dist-tags":{"latest":"1.0.0"},
			"versions":{
				"1.0.0":{
					"name":"toru-fixture-pkg",
					"version":"1.0.0",
					"dist":{
						"tarball":"https://registry.npmjs.org/toru-fixture-pkg/-/toru-fixture-pkg-1.0.0.tgz",
						"integrity":"sha512-deadbeef"
					}
				}
			}
		}`)
	})
	scopedMetadataHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"name":"@toru/fixture-scoped",
			"dist-tags":{"latest":"1.0.0"},
			"versions":{
				"1.0.0":{
					"name":"@toru/fixture-scoped",
					"version":"1.0.0",
					"dist":{
						"tarball":"https://registry.npmjs.org/@toru/fixture-scoped/-/fixture-scoped-1.0.0.tgz",
						"integrity":"sha512-cafebabe"
					}
				}
			}
		}`)
	}
	mux.HandleFunc("/@toru/fixture-scoped", scopedMetadataHandler)
	mux.HandleFunc("/%40toru%2Ffixture-scoped", scopedMetadataHandler)
	mux.HandleFunc("/toru-fixture-pkg/-/toru-fixture-pkg-1.0.0.tgz", func(w http.ResponseWriter, r *http.Request) {
		if tarballHits != nil {
			tarballHits.Add(1)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "fake-tgz-unscoped")
	})
	scopedTarballHandler := func(w http.ResponseWriter, r *http.Request) {
		if tarballHits != nil {
			tarballHits.Add(1)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "fake-tgz-scoped")
	}
	mux.HandleFunc("/@toru/fixture-scoped/-/fixture-scoped-1.0.0.tgz", scopedTarballHandler)
	mux.HandleFunc("/%40toru%2Ffixture-scoped/-/fixture-scoped-1.0.0.tgz", scopedTarballHandler)
	return httptest.NewServer(mux)
}

func startToruProcess(t *testing.T, cfgText string) *exec.Cmd {
	t.Helper()
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", "--config", cfgPath)
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "TORU_TEST_HELPER_PROCESS=1")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start toru helper process: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("toru output:\n%s", output.String())
		}
	})
	return cmd
}

func stopCmd(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("TORU_TEST_HELPER_PROCESS") != "1" {
		return
	}

	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			os.Args = append([]string{"toru"}, args[i+1:]...)
			main()
			return
		}
	}

	os.Exit(2)
}

func waitForHTTP200(t *testing.T, rawURL string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, _, err := getURL(rawURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for 200 from %s", rawURL)
}

func getURL(rawURL string) (*http.Response, string, error) {
	resp, err := http.Get(rawURL)
	if err != nil {
		return nil, "", err
	}
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		_ = resp.Body.Close()
		return nil, "", readErr
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, string(body), nil
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("get free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
