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

type metadataCacheEntry struct {
	Body      []byte
	ETag      string
	ExpiresAt time.Time
	Fresh     bool
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

	if rule, ok := matchNPMRewriteRule(h.cfg.Protocols.NPM.RewriteRules, pkg); ok {
		repoPath, err := repoPathForPackage(rule, pkg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !authorizeRequest(w, r, h.authenticators, rule.AuthModule, AuthRequest{Protocol: "npm", Path: r.URL.Path, Resource: pkg, Scope: rule.Scope, RepoPath: repoPath}) {
			return
		}
		body, statusCode, err := h.synthesizeRewrittenMetadata(r, pkg, rule)
		if err != nil {
			http.Error(w, err.Error(), statusCode)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		finalSize = len(body)
		_, _ = w.Write(body)
		return
	}

	if rule, ok := matchProtectedScope(h.cfg.Protocols.NPM.ProtectedScopes, pkg); ok {
		if !authorizeRequest(w, r, h.authenticators, rule.AuthModule, AuthRequest{Protocol: "npm", Path: r.URL.Path, Resource: pkg, Scope: rule.Scope}) {
			return
		}
	}

	if entry, ok := h.readMetadataCache(pkg); ok {
		if entry.Fresh {
			recordProtocolCacheHit("npm", "metadata")
			rewritten, err := rewriteNPMMetadataTarballs(entry.Body, h.cfg.Protocols.NPM.BaseURL, pkg)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			finalSize = len(rewritten)
			_, _ = w.Write(rewritten)
			return
		}

		if entry.ETag != "" {
			revalidated, statusCode, err := h.revalidateMetadataCache(r, pkg, entry)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			switch statusCode {
			case http.StatusNotModified:
				recordProtocolCacheHit("npm", "metadata")
				rewritten, err := rewriteNPMMetadataTarballs(revalidated.Body, h.cfg.Protocols.NPM.BaseURL, pkg)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadGateway)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				finalSize = len(rewritten)
				_, _ = w.Write(rewritten)
				return
			case http.StatusOK:
				recordProtocolCacheMiss("npm", "metadata")
				rewritten, err := rewriteNPMMetadataTarballs(revalidated.Body, h.cfg.Protocols.NPM.BaseURL, pkg)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadGateway)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				finalSize = len(rewritten)
				_, _ = w.Write(rewritten)
				return
			default:
				recordProtocolCacheMiss("npm", "metadata")
				w.WriteHeader(statusCode)
				finalSize = len(revalidated.Body)
				_, _ = w.Write(revalidated.Body)
				return
			}
		}
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
	wroteMetadataCache, err := h.writeMetadataCache(pkg, body, resp.Header.Get("ETag"))
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

	if rule, ok := matchNPMRewriteRule(h.cfg.Protocols.NPM.RewriteRules, pkg); ok {
		repoPath, err := repoPathForPackage(rule, pkg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !authorizeRequest(w, r, h.authenticators, rule.AuthModule, AuthRequest{Protocol: "npm", Path: r.URL.Path, Resource: pkg, Scope: rule.Scope, RepoPath: repoPath}) {
			return
		}
		gitlabToken := ""
		_, gitlabToken, _ = authorizeRequestToken(r, rule.AuthModule)
		cachePath := h.rewriteTarballCachePath(rule, pkg, filename)
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
		body, statusCode, err := h.synthesizeRewrittenTarball(r, pkg, filename, rule, gitlabToken)
		if err != nil {
			http.Error(w, err.Error(), statusCode)
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
		return
	}

	if rule, ok := matchProtectedScope(h.cfg.Protocols.NPM.ProtectedScopes, pkg); ok {
		if !authorizeRequest(w, r, h.authenticators, rule.AuthModule, AuthRequest{Protocol: "npm", Path: r.URL.Path, Resource: pkg, Scope: rule.Scope}) {
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
	return h.doAuthorizedUpstreamGet(r, upstreamURL, "")
}

func (h *npmHandler) doAuthorizedUpstreamGet(r *http.Request, upstreamURL, bearerToken string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		return nil, nil, err
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
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

func (h *npmHandler) metadataETagPath(pkg string) string {
	return filepath.Join(h.cfg.Cache.Disk.Path, "npm-meta", npmCachePackageKey(pkg)+".etag")
}

func (h *npmHandler) metadataExpiryPath(pkg string) string {
	return filepath.Join(h.cfg.Cache.Disk.Path, "npm-meta", npmCachePackageKey(pkg)+".expiry")
}

func (h *npmHandler) tarballCachePath(pkg, filename string) string {
	if !h.artifactCacheEnabled() {
		return ""
	}
	return filepath.Join(h.cfg.Cache.Disk.Path, "npm", npmCachePackageKey(pkg), filename)
}

func (h *npmHandler) rewriteTarballCachePath(rule NPMRewriteRule, pkg, filename string) string {
	if !h.artifactCacheEnabled() {
		return ""
	}
	hostKey := strings.TrimSpace(rule.TargetHost)
	hostKey = strings.ReplaceAll(hostKey, ":", "_")
	groupKey := strings.ReplaceAll(normalizeTargetGroup(rule.TargetGroup), "/", "__")
	return filepath.Join(h.cfg.Cache.Disk.Path, "npm-rewrite", hostKey, groupKey, npmCachePackageKey(pkg), filename)
}

func (h *npmHandler) readMetadataCache(pkg string) (metadataCacheEntry, bool) {
	if !h.metadataCacheEnabled() {
		return metadataCacheEntry{}, false
	}
	path := h.metadataCachePath(pkg)
	info, err := os.Stat(path)
	if err != nil {
		return metadataCacheEntry{}, false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return metadataCacheEntry{}, false
	}
	etagBytes, _ := os.ReadFile(h.metadataETagPath(pkg))
	expiresAt := info.ModTime().Add(h.cfg.Protocols.NPM.MetadataTTL)
	if cachedExpiry, ok := h.readMetadataExpiry(pkg); ok {
		expiresAt = cachedExpiry
	}
	return metadataCacheEntry{
		Body:      body,
		ETag:      string(etagBytes),
		ExpiresAt: expiresAt,
		Fresh:     time.Now().Before(expiresAt),
	}, true
}

func (h *npmHandler) writeMetadataCache(pkg string, body []byte, etag string) (bool, error) {
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
	etagPath := h.metadataETagPath(pkg)
	if etag != "" {
		if err := os.WriteFile(etagPath, []byte(etag), 0o644); err != nil {
			h.logger.Error("failed to write npm metadata etag file; serving uncached body", "path", etagPath, "error", err)
			return false, err
		}
	} else {
		_ = os.Remove(etagPath)
	}
	if err := h.writeMetadataExpiry(pkg, time.Now().Add(h.cfg.Protocols.NPM.MetadataTTL)); err != nil {
		h.logger.Error("failed to write npm metadata expiry file; serving uncached body", "path", h.metadataExpiryPath(pkg), "error", err)
		return false, err
	}
	return true, nil
}

func (h *npmHandler) readMetadataExpiry(pkg string) (time.Time, bool) {
	b, err := os.ReadFile(h.metadataExpiryPath(pkg))
	if err != nil {
		return time.Time{}, false
	}
	ns, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}, false
	}
	return ns, true
}

func (h *npmHandler) writeMetadataExpiry(pkg string, expiresAt time.Time) error {
	return os.WriteFile(h.metadataExpiryPath(pkg), []byte(expiresAt.UTC().Format(time.RFC3339Nano)), 0o644)
}

func (h *npmHandler) revalidateMetadataCache(r *http.Request, pkg string, entry metadataCacheEntry) (metadataCacheEntry, int, error) {
	upstreamURL := strings.TrimSuffix(h.cfg.Protocols.NPM.Upstream, "/") + "/" + encodeNPMPackagePath(pkg)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		return metadataCacheEntry{}, 0, err
	}
	req.Header.Set("If-None-Match", entry.ETag)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return metadataCacheEntry{}, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return metadataCacheEntry{}, 0, err
	}
	if resp.StatusCode == http.StatusNotModified {
		refreshedExpiry := time.Now().Add(h.cfg.Protocols.NPM.MetadataTTL)
		if err := h.writeMetadataExpiry(pkg, refreshedExpiry); err != nil {
			return metadataCacheEntry{}, 0, err
		}
		return metadataCacheEntry{Body: entry.Body, ETag: entry.ETag, ExpiresAt: refreshedExpiry, Fresh: true}, http.StatusNotModified, nil
	}
	if resp.StatusCode != http.StatusOK {
		return metadataCacheEntry{Body: body}, resp.StatusCode, nil
	}
	_, err = h.writeMetadataCache(pkg, body, resp.Header.Get("ETag"))
	if err != nil {
		return metadataCacheEntry{}, 0, err
	}
	return metadataCacheEntry{Body: body, ETag: resp.Header.Get("ETag"), ExpiresAt: time.Now().Add(h.cfg.Protocols.NPM.MetadataTTL), Fresh: true}, http.StatusOK, nil
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
