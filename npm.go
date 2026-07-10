package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type npmHandler struct {
	cfg            *Config
	logger         *slog.Logger
	httpClient     *http.Client
	authenticators map[string]Authenticator
}

func newNPMHandler(cfg *Config, logger *slog.Logger, authenticators map[string]Authenticator) http.Handler {
	timeout := cfg.Server.FetchTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &npmHandler{
		cfg:            cfg,
		logger:         logger.With("component", "npm"),
		httpClient:     &http.Client{Timeout: timeout},
		authenticators: authenticators,
	}
}

func (h *npmHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.Contains(r.URL.Path, "/-/") {
		h.handleTarball(w, r)
		return
	}
	h.handleMetadata(w, r)
}

func (h *npmHandler) handleMetadata(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	pkg := decodeNPMPackagePath(r.URL.Path)
	finalSize := 0
	defer func() { recordProtocolRequest("npm", "metadata", time.Since(startTime), finalSize) }()

	if pkg == "" {
		http.Error(w, "invalid package path", http.StatusBadRequest)
		return
	}
	h.logger.Info("Received request", "protocol", "npm", "kind", "metadata", "method", r.Method, "path", r.URL.Path, "package", pkg)

	if rule, ok := matchProtectedScope(h.cfg.Protocols.NPM.ProtectedScopes, pkg); ok {
		if !authorizeRequest(w, r, h.authenticators, rule.AuthModule, AuthRequest{Protocol: "npm", Path: r.URL.Path, Resource: pkg}) {
			return
		}
	}

	if body, ok := h.readFreshMetadataCache(pkg); ok {
		recordProtocolCacheHit("npm", "metadata")
		rewritten, err := rewriteNPMMetadataTarballs(body, h.cfg.Protocols.NPM.BaseURL, pkg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		finalSize = len(rewritten)
		_, _ = w.Write(rewritten)
		return
	}

	recordProtocolCacheMiss("npm", "metadata")
	upstreamURL := strings.TrimSuffix(h.cfg.Protocols.NPM.Upstream, "/") + "/" + encodeNPMPackagePath(pkg)
	resp, body, err := h.doUpstreamGet(r, upstreamURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		finalSize = len(body)
		_, _ = w.Write(body)
		return
	}
	wroteMetadataCache, err := h.writeMetadataCache(pkg, body)
	if err == nil {
		if wroteMetadataCache {
			recordProtocolCacheWrite("npm", "metadata")
		}
	} else {
		recordProtocolCacheError("npm", "metadata")
	}
	rewritten, err := rewriteNPMMetadataTarballs(body, h.cfg.Protocols.NPM.BaseURL, pkg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	finalSize = len(rewritten)
	_, _ = w.Write(rewritten)
}

func (h *npmHandler) handleTarball(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	pkg, filename := parseNPMTarballPath(r.URL.Path)
	finalSize := 0
	defer func() { recordProtocolRequest("npm", "artifact", time.Since(startTime), finalSize) }()

	if pkg == "" || filename == "" {
		http.Error(w, "invalid tarball path", http.StatusBadRequest)
		return
	}
	h.logger.Info("Received request", "protocol", "npm", "kind", "artifact", "method", r.Method, "path", r.URL.Path, "package", pkg, "filename", filename)

	if rule, ok := matchProtectedScope(h.cfg.Protocols.NPM.ProtectedScopes, pkg); ok {
		if !authorizeRequest(w, r, h.authenticators, rule.AuthModule, AuthRequest{Protocol: "npm", Path: r.URL.Path, Resource: pkg}) {
			return
		}
	}

	cachePath := h.tarballCachePath(pkg, filename)
	if cachePath != "" {
		if body, err := os.ReadFile(cachePath); err == nil {
			recordProtocolCacheHit("npm", "artifact")
			w.Header().Set("Content-Type", "application/octet-stream")
			finalSize = len(body)
			_, _ = w.Write(body)
			return
		}
		recordProtocolCacheMiss("npm", "artifact")
	}

	upstreamURL := strings.TrimSuffix(h.cfg.Protocols.NPM.Upstream, "/") + "/" + pkg + "/-/" + filename
	resp, body, err := h.doUpstreamGet(r, upstreamURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		finalSize = len(body)
		_, _ = w.Write(body)
		return
	}
	if cachePath != "" {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
			h.logger.Error("failed to create npm cache directory; serving uncached body", "path", filepath.Dir(cachePath), "error", err)
			recordProtocolCacheError("npm", "artifact")
			w.Header().Set("Content-Type", "application/octet-stream")
			finalSize = len(body)
			_, _ = w.Write(body)
			return
		}
		if err := os.WriteFile(cachePath, body, 0o644); err != nil {
			h.logger.Error("failed to write npm cache file; serving uncached body", "path", cachePath, "error", err)
			recordProtocolCacheError("npm", "artifact")
			w.Header().Set("Content-Type", "application/octet-stream")
			finalSize = len(body)
			_, _ = w.Write(body)
			return
		}
		recordProtocolCacheWrite("npm", "artifact")
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	finalSize = len(body)
	_, _ = w.Write(body)
}

func (h *npmHandler) doUpstreamGet(r *http.Request, upstreamURL string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		resp.Body.Close()
		return nil, nil, err
	}
	resp.Body = io.NopCloser(strings.NewReader(string(body)))
	return resp, body, nil
}

func decodeNPMPackagePath(path string) string {
	path = strings.TrimPrefix(path, "/")
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return path
	}
	return decoded
}

func encodeNPMPackagePath(pkg string) string {
	encoded := url.PathEscape(pkg)
	return strings.ReplaceAll(encoded, "@", "%40")
}

func npmCachePackageKey(pkg string) string {
	return encodeNPMPackagePath(pkg)
}

func parseNPMTarballPath(path string) (string, string) {
	trimmed := strings.TrimPrefix(path, "/")
	idx := strings.Index(trimmed, "/-/")
	if idx < 0 {
		return "", ""
	}
	pkg := decodeNPMPackagePath(trimmed[:idx])
	return pkg, trimmed[idx+3:]
}

func (h *npmHandler) metadataCacheEnabled() bool {
	return h.cfg.Cache.Enabled && h.cfg.Cache.Type == "disk" && h.cfg.Cache.Disk.Path != "" && h.cfg.Protocols.NPM.MetadataTTL > 0
}

func (h *npmHandler) artifactCacheEnabled() bool {
	return h.cfg.Cache.Enabled && h.cfg.Cache.Type == "disk" && h.cfg.Cache.Disk.Path != ""
}

func (h *npmHandler) metadataCachePath(pkg string) string {
	return filepath.Join(h.cfg.Cache.Disk.Path, "npm-meta", npmCachePackageKey(pkg)+".json")
}

func (h *npmHandler) tarballCachePath(pkg, filename string) string {
	if !h.artifactCacheEnabled() {
		return ""
	}
	return filepath.Join(h.cfg.Cache.Disk.Path, "npm", npmCachePackageKey(pkg), filename)
}

func (h *npmHandler) readFreshMetadataCache(pkg string) ([]byte, bool) {
	if !h.metadataCacheEnabled() {
		return nil, false
	}
	path := h.metadataCachePath(pkg)
	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	if time.Since(info.ModTime()) > h.cfg.Protocols.NPM.MetadataTTL {
		return nil, false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return body, true
}

func (h *npmHandler) writeMetadataCache(pkg string, body []byte) (bool, error) {
	if !h.metadataCacheEnabled() {
		return false, nil
	}
	path := h.metadataCachePath(pkg)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Error("failed to create npm metadata cache directory; serving uncached body", "path", filepath.Dir(path), "error", err)
		return false, err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		h.logger.Error("failed to write npm metadata cache file; serving uncached body", "path", path, "error", err)
		return false, err
	}
	return true, nil
}

func rewriteNPMMetadataTarballs(body []byte, baseURL, packageName string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	encodedPackageName := encodeNPMPackagePath(packageName)
	versions, _ := doc["versions"].(map[string]any)
	for _, value := range versions {
		vm, ok := value.(map[string]any)
		if !ok {
			continue
		}
		dist, ok := vm["dist"].(map[string]any)
		if !ok {
			continue
		}
		tarball, ok := dist["tarball"].(string)
		if !ok {
			continue
		}
		parts := strings.Split(tarball, "/")
		filename := parts[len(parts)-1]
		dist["tarball"] = fmt.Sprintf("%s/%s/-/%s", strings.TrimSuffix(baseURL, "/"), encodedPackageName, filename)
	}
	return json.Marshal(doc)
}
