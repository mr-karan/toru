package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeGitLabRepo struct {
	Tags          []string
	TagTargets    map[string]string
	Archives      map[string]map[string]string
	ArchiveHits   *atomic.Int32
	AllowedTokens map[string]bool
}

type fakeGitLabRegistry struct {
	server *httptest.Server
}

func newFakeGitLabRegistry(t *testing.T, repos map[string]fakeGitLabRepo) *fakeGitLabRegistry {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		trimmed := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/")
		if !strings.Contains(trimmed, "/repository/") {
			projectPath, err := url.PathUnescape(strings.Trim(trimmed, "/"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			repo, ok := repos[projectPath]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			if !fakeGitLabTokenAllowed(repo, r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"path_with_namespace":"`+projectPath+`"}`)
			return
		}
		if strings.HasSuffix(trimmed, "/repository/tags") {
			projectPath, err := url.PathUnescape(strings.TrimSuffix(trimmed, "/repository/tags"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			repo, ok := repos[projectPath]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			if !fakeGitLabTokenAllowed(repo, r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			payload := make([]map[string]string, 0, len(repo.Tags))
			for _, tag := range repo.Tags {
				entry := map[string]string{"name": tag}
				if target := repo.TagTargets[tag]; target != "" {
					entry["target"] = target
				}
				payload = append(payload, entry)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(payload)
			return
		}
		if strings.HasSuffix(trimmed, "/repository/archive.tar.gz") {
			projectPath, err := url.PathUnescape(strings.TrimSuffix(trimmed, "/repository/archive.tar.gz"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			repo, ok := repos[projectPath]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			if !fakeGitLabTokenAllowed(repo, r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			tag := r.URL.Query().Get("sha")
			files, ok := repo.Archives[tag]
			if !ok {
				http.Error(w, "archive not found", http.StatusNotFound)
				return
			}
			if repo.ArchiveHits != nil {
				repo.ArchiveHits.Add(1)
			}
			archive, err := makeGitLabArchive(filepath.Base(projectPath)+"-"+tag, files)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archive)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	return &fakeGitLabRegistry{server: httptest.NewServer(mux)}
}

func (f *fakeGitLabRegistry) Close()       { f.server.Close() }
func (f *fakeGitLabRegistry) Host() string { return strings.TrimPrefix(f.server.URL, "http://") }

func fakeGitLabTokenAllowed(repo fakeGitLabRepo, r *http.Request) bool {
	if len(repo.AllowedTokens) == 0 {
		return true
	}
	if token := strings.TrimSpace(r.Header.Get("PRIVATE-TOKEN")); token != "" {
		return repo.AllowedTokens[token]
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		token := strings.TrimSpace(auth[len("Bearer "):])
		return repo.AllowedTokens[token]
	}
	return false
}

func readNPMTarballFiles(body []byte) (map[string]string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[hdr.Name] = string(data)
	}
	return files, nil
}

func makeGitLabArchive(root string, files map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		data := []byte(content)
		hdr := &tar.Header{Name: root + "/" + name, Mode: 0o644, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func TestNPMRewriteMetadataUsesGitLabTokenAgainstDerivedRepoPath(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)
	gitlab := newFakeGitLabRegistry(t, map[string]fakeGitLabRepo{
		"platform/commons/foo": {
			Tags: []string{"v1.0.0"},
			Archives: map[string]map[string]string{
				"v1.0.0": {"package.json": `{"name":"@example-commons/foo"}`},
			},
			AllowedTokens: map[string]bool{"good-token": true},
		},
	})
	defer gitlab.Close()

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

[auth]
enabled = true

[[auth.modules]]
name = "gitlab"
type = "gitlab_access_token"
options.root_url = "http://%s"
options.project_prefix = "wrong/prefix"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = %q
metadata_ttl = "5m"
base_url = "http://127.0.0.1:%d"

[[protocols.npm.rewrite_rules]]
scope = "@example-commons"
target_host = %q
target_group = "platform/commons"
auth_module = "gitlab"
`, goPort, goPort, npmPort, filepath.Join(t.TempDir(), "cache"), gitlab.Host(), upstream.URL, npmPort, gitlab.Host())
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)
	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/%%40example-commons%%2Ffoo", npmPort), nil)
	req.SetBasicAuth("gitlab", "good-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request derived-repo metadata: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%q, want 200", resp.StatusCode, string(body))
	}
}

func TestNPMRewriteMetadataRejectsGitLabTokenWithoutDerivedRepoAccess(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)
	gitlab := newFakeGitLabRegistry(t, map[string]fakeGitLabRepo{
		"platform/commons/foo": {
			Tags: []string{"v1.0.0"},
			Archives: map[string]map[string]string{
				"v1.0.0": {"package.json": `{"name":"@example-commons/foo"}`},
			},
			AllowedTokens: map[string]bool{"good-token": true},
		},
	})
	defer gitlab.Close()

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

[auth]
enabled = true

[[auth.modules]]
name = "gitlab"
type = "gitlab_access_token"
options.root_url = "http://%s"
options.project_prefix = "wrong/prefix"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = %q
metadata_ttl = "5m"
base_url = "http://127.0.0.1:%d"

[[protocols.npm.rewrite_rules]]
scope = "@example-commons"
target_host = %q
target_group = "platform/commons"
auth_module = "gitlab"
`, goPort, goPort, npmPort, filepath.Join(t.TempDir(), "cache"), gitlab.Host(), upstream.URL, npmPort, gitlab.Host())
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)
	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/%%40example-commons%%2Ffoo", npmPort), nil)
	req.SetBasicAuth("gitlab", "bad-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request derived-repo metadata: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q, want 403", resp.StatusCode, string(body))
	}
}

func TestNPMRewriteMetadataSynthesizesFromGitLabTags(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)
	gitlab := newFakeGitLabRegistry(t, map[string]fakeGitLabRepo{
		"commons/foo": {
			Tags: []string{"v1.0.0", "v1.2.0", "1.3.0", "main"},
			Archives: map[string]map[string]string{
				"v1.0.0": {"package.json": `{"name":"@example-commons/foo","main":"index.js"}`},
				"v1.2.0": {"package.json": `{"name":"@example-commons/foo","dependencies":{"is-number":"^7.0.0"}}`},
				"1.3.0":  {"package.json": `{"name":"@example-commons/foo","exports":"./index.js"}`},
			},
		},
	})
	defer gitlab.Close()

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

[auth]
enabled = true

[[auth.modules]]
name = "static"
type = "static_token"
options.token = "secret"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = %q
metadata_ttl = "5m"
base_url = "http://127.0.0.1:%d"

[[protocols.npm.rewrite_rules]]
scope = "@example-commons"
target_host = %q
target_group = "commons"
auth_module = "static"
`, goPort, goPort, npmPort, filepath.Join(t.TempDir(), "cache"), upstream.URL, npmPort, gitlab.Host())
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/%%40example-commons%%2Ffoo", npmPort), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.SetBasicAuth("static", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request rewritten metadata: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%q, want 200", resp.StatusCode, string(body))
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if doc["name"] != "@example-commons/foo" {
		t.Fatalf("name=%v, want @example-commons/foo", doc["name"])
	}
	distTags := doc["dist-tags"].(map[string]any)
	if distTags["latest"] != "1.3.0" {
		t.Fatalf("latest=%v, want 1.3.0", distTags["latest"])
	}
	versions := doc["versions"].(map[string]any)
	if len(versions) != 3 {
		t.Fatalf("versions len=%d, want 3", len(versions))
	}
	v130 := versions["1.3.0"].(map[string]any)
	dist := v130["dist"].(map[string]any)
	wantTarball := fmt.Sprintf("http://127.0.0.1:%d/%%40example-commons%%2Ffoo/-/foo-1.3.0.tgz", npmPort)
	if dist["tarball"] != wantTarball {
		t.Fatalf("tarball=%v, want %q", dist["tarball"], wantTarball)
	}
}

func TestNPMRewriteMetadataCoexistsWithUpstreamProxy(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)
	gitlab := newFakeGitLabRegistry(t, map[string]fakeGitLabRepo{
		"commons/foo": {
			Tags: []string{"v1.0.0"},
			Archives: map[string]map[string]string{
				"v1.0.0": {"package.json": `{"name":"@example-commons/foo"}`},
			},
		},
	})
	defer gitlab.Close()

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

[auth]
enabled = true

[[auth.modules]]
name = "static"
type = "static_token"
options.token = "secret"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = %q
metadata_ttl = "5m"
base_url = "http://127.0.0.1:%d"

[[protocols.npm.rewrite_rules]]
scope = "@example-commons"
target_host = %q
target_group = "commons"
auth_module = "static"
`, goPort, goPort, npmPort, filepath.Join(t.TempDir(), "cache"), upstream.URL, npmPort, gitlab.Host())
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/%%40example-commons%%2Ffoo", npmPort), nil)
	req.SetBasicAuth("static", "secret")
	respRewrite, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request rewritten metadata: %v", err)
	}
	respRewrite.Body.Close()
	if respRewrite.StatusCode != http.StatusOK {
		t.Fatalf("rewrite status=%d, want 200", respRewrite.StatusCode)
	}

	respPublic, bodyPublic, err := getURL(fmt.Sprintf("http://127.0.0.1:%d/toru-fixture-pkg", npmPort))
	if err != nil {
		t.Fatalf("request public metadata: %v", err)
	}
	defer respPublic.Body.Close()
	if respPublic.StatusCode != http.StatusOK {
		t.Fatalf("public status=%d body=%q, want 200", respPublic.StatusCode, bodyPublic)
	}
}

func TestNPMRewriteMetadataRejectsManifestNameMismatch(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)
	gitlab := newFakeGitLabRegistry(t, map[string]fakeGitLabRepo{
		"commons/bad-name": {
			Tags: []string{"v1.0.0"},
			Archives: map[string]map[string]string{
				"v1.0.0": {"package.json": `{"name":"@someone-else/bad-name"}`},
			},
		},
	})
	defer gitlab.Close()

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

[auth]
enabled = true

[[auth.modules]]
name = "static"
type = "static_token"
options.token = "secret"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = %q
metadata_ttl = "5m"
base_url = "http://127.0.0.1:%d"

[[protocols.npm.rewrite_rules]]
scope = "@example-commons"
target_host = %q
target_group = "commons"
auth_module = "static"
`, goPort, goPort, npmPort, filepath.Join(t.TempDir(), "cache"), upstream.URL, npmPort, gitlab.Host())
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/%%40example-commons%%2Fbad-name", npmPort), nil)
	req.SetBasicAuth("static", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request mismatch metadata: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d body=%q, want 502", resp.StatusCode, string(body))
	}
}

func TestNPMRewriteMetadataPrefersExactTagWhenDuplicateTargetsMatch(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)
	gitlab := newFakeGitLabRegistry(t, map[string]fakeGitLabRepo{
		"commons/foo": {
			Tags: []string{"v1.2.3", "1.2.3"},
			TagTargets: map[string]string{
				"v1.2.3": "abc123",
				"1.2.3":  "abc123",
			},
			Archives: map[string]map[string]string{
				"1.2.3":  {"package.json": `{"name":"@example-commons/foo"}`},
				"v1.2.3": {"package.json": `{"name":"@example-commons/foo"}`},
			},
		},
	})
	defer gitlab.Close()

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

[auth]
enabled = true

[[auth.modules]]
name = "static"
type = "static_token"
options.token = "secret"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = %q
metadata_ttl = "5m"
base_url = "http://127.0.0.1:%d"

[[protocols.npm.rewrite_rules]]
scope = "@example-commons"
target_host = %q
target_group = "commons"
auth_module = "static"
`, goPort, goPort, npmPort, filepath.Join(t.TempDir(), "cache"), upstream.URL, npmPort, gitlab.Host())
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/%%40example-commons%%2Ffoo", npmPort), nil)
	req.SetBasicAuth("static", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request duplicate-target metadata: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%q, want 200", resp.StatusCode, string(body))
	}
}

func TestNPMRewriteMetadataRejectsAmbiguousDuplicateVersionTags(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)
	gitlab := newFakeGitLabRegistry(t, map[string]fakeGitLabRepo{
		"commons/foo": {
			Tags: []string{"v1.2.3", "1.2.3"},
			TagTargets: map[string]string{
				"v1.2.3": "abc123",
				"1.2.3":  "def456",
			},
			Archives: map[string]map[string]string{
				"1.2.3":  {"package.json": `{"name":"@example-commons/foo"}`},
				"v1.2.3": {"package.json": `{"name":"@example-commons/foo"}`},
			},
		},
	})
	defer gitlab.Close()

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

[auth]
enabled = true

[[auth.modules]]
name = "static"
type = "static_token"
options.token = "secret"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = %q
metadata_ttl = "5m"
base_url = "http://127.0.0.1:%d"

[[protocols.npm.rewrite_rules]]
scope = "@example-commons"
target_host = %q
target_group = "commons"
auth_module = "static"
`, goPort, goPort, npmPort, filepath.Join(t.TempDir(), "cache"), upstream.URL, npmPort, gitlab.Host())
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/%%40example-commons%%2Ffoo", npmPort), nil)
	req.SetBasicAuth("static", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request ambiguous duplicate metadata: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d body=%q, want 502", resp.StatusCode, string(body))
	}
}

func TestNPMRewriteTarballSynthesizesPackagePrefixAndCaches(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)
	var archiveHits atomic.Int32
	gitlab := newFakeGitLabRegistry(t, map[string]fakeGitLabRepo{
		"commons/foo": {
			Tags: []string{"v1.2.0"},
			Archives: map[string]map[string]string{
				"v1.2.0": {
					"package.json": `{"name":"@example-commons/foo","main":"index.js"}`,
					"index.js":     `module.exports = 42`,
				},
			},
			ArchiveHits: &archiveHits,
		},
	})
	defer gitlab.Close()

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

[auth]
enabled = true

[[auth.modules]]
name = "static"
type = "static_token"
options.token = "secret"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = %q
metadata_ttl = "5m"
base_url = "http://127.0.0.1:%d"

[[protocols.npm.rewrite_rules]]
scope = "@example-commons"
target_host = %q
target_group = "commons"
auth_module = "static"
`, goPort, goPort, npmPort, filepath.Join(t.TempDir(), "cache"), upstream.URL, npmPort, gitlab.Host())
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	url := fmt.Sprintf("http://127.0.0.1:%d/%%40example-commons%%2Ffoo/-/foo-1.2.0.tgz", npmPort)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.SetBasicAuth("static", "secret")
	resp1, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("first tarball request: %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first tarball status=%d body=%q, want 200", resp1.StatusCode, string(body1))
	}
	if hits := archiveHits.Load(); hits != 1 {
		t.Fatalf("archive hits after first request = %d, want 1", hits)
	}
	files, err := readNPMTarballFiles(body1)
	if err != nil {
		t.Fatalf("read synthesized tarball: %v", err)
	}
	if _, ok := files["package/package.json"]; !ok {
		t.Fatalf("expected package/package.json in synthesized tarball, files=%v", files)
	}
	if _, ok := files["package/index.js"]; !ok {
		t.Fatalf("expected package/index.js in synthesized tarball, files=%v", files)
	}

	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("second tarball request: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second tarball status=%d body=%q, want 200", resp2.StatusCode, string(body2))
	}
	if hits := archiveHits.Load(); hits != 1 {
		t.Fatalf("archive hits after cached request = %d, want still 1", hits)
	}
}

func TestNPMRewriteTarballFailsWhenRootPackageJSONMissing(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)
	gitlab := newFakeGitLabRegistry(t, map[string]fakeGitLabRepo{
		"commons/missing-manifest": {
			Tags: []string{"v1.0.0"},
			Archives: map[string]map[string]string{
				"v1.0.0": {"README.md": "hello"},
			},
		},
	})
	defer gitlab.Close()

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

[auth]
enabled = true

[[auth.modules]]
name = "static"
type = "static_token"
options.token = "secret"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = %q
metadata_ttl = "5m"
base_url = "http://127.0.0.1:%d"

[[protocols.npm.rewrite_rules]]
scope = "@example-commons"
target_host = %q
target_group = "commons"
auth_module = "static"
`, goPort, goPort, npmPort, filepath.Join(t.TempDir(), "cache"), upstream.URL, npmPort, gitlab.Host())
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/%%40example-commons%%2Fmissing-manifest/-/missing-manifest-1.0.0.tgz", npmPort), nil)
	req.SetBasicAuth("static", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request missing-manifest tarball: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d body=%q, want 502", resp.StatusCode, string(body))
	}
}

func TestNPMRewriteTarballUsesSeparateCacheNamespaceFromUpstream(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)
	var archiveHits atomic.Int32
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	gitlab := newFakeGitLabRegistry(t, map[string]fakeGitLabRepo{
		"commons/foo": {
			Tags: []string{"v1.2.0"},
			Archives: map[string]map[string]string{
				"v1.2.0": {
					"package.json": `{"name":"@example-commons/foo","main":"index.js"}`,
					"index.js":     `module.exports = 42`,
				},
			},
			ArchiveHits: &archiveHits,
		},
	})
	defer gitlab.Close()

	if err := os.MkdirAll(filepath.Join(cacheRoot, "npm", "%40example-commons%2Ffoo"), 0o755); err != nil {
		t.Fatalf("mkdir upstream cache path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheRoot, "npm", "%40example-commons%2Ffoo", "foo-1.2.0.tgz"), []byte("upstream-body"), 0o644); err != nil {
		t.Fatalf("seed upstream cache path: %v", err)
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

[auth]
enabled = true

[[auth.modules]]
name = "static"
type = "static_token"
options.token = "secret"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = %q
metadata_ttl = "5m"
base_url = "http://127.0.0.1:%d"

[[protocols.npm.rewrite_rules]]
scope = "@example-commons"
target_host = %q
target_group = "commons"
auth_module = "static"
`, goPort, goPort, npmPort, cacheRoot, upstream.URL, npmPort, gitlab.Host())
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	url := fmt.Sprintf("http://127.0.0.1:%d/%%40example-commons%%2Ffoo/-/foo-1.2.0.tgz", npmPort)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.SetBasicAuth("static", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request rewrite tarball: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%q, want 200", resp.StatusCode, string(body))
	}
	if string(body) == "upstream-body" {
		t.Fatalf("rewrite tarball served stale upstream cache body")
	}
	if hits := archiveHits.Load(); hits != 1 {
		t.Fatalf("archive hits after rewrite tarball request = %d, want 1", hits)
	}
	hostKey := strings.ReplaceAll(gitlab.Host(), ":", "_")
	if _, err := os.Stat(filepath.Join(cacheRoot, "npm-rewrite", hostKey, "commons", npmCachePackageKey("@example-commons/foo"), "foo-1.2.0.tgz")); err != nil {
		t.Fatalf("expected rewrite cache artifact in isolated namespace: %v", err)
	}
}

func TestNPMRewriteMetadataFailsWhenRootPackageJSONMissing(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)
	gitlab := newFakeGitLabRegistry(t, map[string]fakeGitLabRepo{
		"commons/missing-manifest": {
			Tags: []string{"v1.0.0"},
			Archives: map[string]map[string]string{
				"v1.0.0": {"README.md": "hello"},
			},
		},
	})
	defer gitlab.Close()

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

[auth]
enabled = true

[[auth.modules]]
name = "static"
type = "static_token"
options.token = "secret"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = %q
metadata_ttl = "5m"
base_url = "http://127.0.0.1:%d"

[[protocols.npm.rewrite_rules]]
scope = "@example-commons"
target_host = %q
target_group = "commons"
auth_module = "static"
`, goPort, goPort, npmPort, filepath.Join(t.TempDir(), "cache"), upstream.URL, npmPort, gitlab.Host())
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/%%40example-commons%%2Fmissing-manifest", npmPort), nil)
	req.SetBasicAuth("static", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request missing-manifest metadata: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d body=%q, want 502", resp.StatusCode, string(body))
	}
}

func TestNPMRewriteMetadataReturns404WithoutValidSemverTags(t *testing.T) {
	goPort := freePort(t)
	npmPort := freePort(t)
	upstream := newFakeNPMRegistry(t)
	gitlab := newFakeGitLabRegistry(t, map[string]fakeGitLabRepo{
		"utils/no-tags": {
			Tags:     []string{"main", "release-candidate"},
			Archives: map[string]map[string]string{},
		},
	})
	defer gitlab.Close()

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

[auth]
enabled = true

[[auth.modules]]
name = "static"
type = "static_token"
options.token = "secret"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = %q
metadata_ttl = "5m"
base_url = "http://127.0.0.1:%d"

[[protocols.npm.rewrite_rules]]
scope = "@example-utils"
target_host = %q
target_group = "utils"
auth_module = "static"
`, goPort, goPort, npmPort, filepath.Join(t.TempDir(), "cache"), upstream.URL, npmPort, gitlab.Host())
	proc := startToruProcess(t, cfgText)
	defer stopCmd(t, proc)

	waitForHTTP200(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", goPort), 10*time.Second)

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/%%40zerodha-utils%%2Fno-tags", npmPort), nil)
	req.SetBasicAuth("static", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request no-tags metadata: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d body=%q, want 404", resp.StatusCode, string(body))
	}
}
