package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

type npmHandler struct {
	cfg            *Config
	logger         *slog.Logger
	httpClient     *http.Client
	authenticators map[string]Authenticator
	npmCache       npmCacheStore
	now            func() time.Time
}

type metadataCacheEntry struct {
	Body      []byte
	ETag      string
	ExpiresAt time.Time
	Fresh     bool
}

func newNPMHandler(cfg *Config, logger *slog.Logger, authenticators map[string]Authenticator) (http.Handler, error) {
	timeout := cfg.Server.FetchTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	cacheStore, err := newNPMCacheStore(cfg)
	if err != nil {
		return nil, err
	}
	return &npmHandler{
		cfg:            cfg,
		logger:         logger.With("component", "npm"),
		httpClient:     &http.Client{Timeout: timeout},
		authenticators: authenticators,
		npmCache:       cacheStore,
		now:            time.Now,
	}, nil
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
		cacheKey := h.rewriteTarballCacheKey(rule, pkg, filename)
		if cacheKey != "" {
			if body, _, ok, err := h.readCacheBytes(cacheKey); err == nil && ok {
				recordProtocolCacheHit("npm", "artifact")
				w.Header().Set("Content-Type", "application/octet-stream")
				finalSize = len(body)
				_, _ = w.Write(body)
				return
			} else if err != nil {
				h.logger.Error("failed to read npm cache body; falling back to uncached fetch", "key", cacheKey, "error", err)
				recordProtocolCacheError("npm", "artifact")
			} else {
				recordProtocolCacheMiss("npm", "artifact")
			}
		}
		body, statusCode, err := h.synthesizeRewrittenTarball(r, pkg, filename, rule, gitlabToken)
		if err != nil {
			http.Error(w, err.Error(), statusCode)
			return
		}
		if cacheKey != "" {
			if err := h.writeCacheBytes(cacheKey, body); err != nil {
				h.logger.Error("failed to write npm cache body; serving uncached body", "key", cacheKey, "error", err)
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

	cacheKey := h.tarballCacheKey(pkg, filename)
	if cacheKey != "" {
		if body, _, ok, err := h.readCacheBytes(cacheKey); err == nil && ok {
			recordProtocolCacheHit("npm", "artifact")
			w.Header().Set("Content-Type", "application/octet-stream")
			finalSize = len(body)
			_, _ = w.Write(body)
			return
		} else if err != nil {
			h.logger.Error("failed to read npm cache body; falling back to uncached fetch", "key", cacheKey, "error", err)
			recordProtocolCacheError("npm", "artifact")
		} else {
			recordProtocolCacheMiss("npm", "artifact")
		}
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
	if cacheKey != "" {
		if err := h.writeCacheBytes(cacheKey, body); err != nil {
			h.logger.Error("failed to write npm cache body; serving uncached body", "key", cacheKey, "error", err)
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
	return h.npmCache != nil && h.cfg.Protocols.NPM.MetadataTTL > 0
}

func (h *npmHandler) artifactCacheEnabled() bool {
	return h.npmCache != nil
}

func (h *npmHandler) metadataCachePath(pkg string) string {
	return filepath.Join(h.cfg.Cache.Disk.Path, h.metadataCacheKey(pkg))
}

func (h *npmHandler) metadataETagPath(pkg string) string {
	return filepath.Join(h.cfg.Cache.Disk.Path, h.metadataETagKey(pkg))
}

func (h *npmHandler) metadataExpiryPath(pkg string) string {
	return filepath.Join(h.cfg.Cache.Disk.Path, h.metadataExpiryKey(pkg))
}

func (h *npmHandler) metadataCacheKey(pkg string) string {
	return filepath.ToSlash(filepath.Join("npm-meta", npmCachePackageKey(pkg)+".json"))
}

func (h *npmHandler) metadataETagKey(pkg string) string {
	return filepath.ToSlash(filepath.Join("npm-meta", npmCachePackageKey(pkg)+".etag"))
}

func (h *npmHandler) metadataExpiryKey(pkg string) string {
	return filepath.ToSlash(filepath.Join("npm-meta", npmCachePackageKey(pkg)+".expiry"))
}

func (h *npmHandler) tarballCachePath(pkg, filename string) string {
	return filepath.Join(h.cfg.Cache.Disk.Path, h.tarballCacheKey(pkg, filename))
}

func (h *npmHandler) tarballCacheKey(pkg, filename string) string {
	if !h.artifactCacheEnabled() {
		return ""
	}
	return filepath.ToSlash(filepath.Join("npm", npmCachePackageKey(pkg), filename))
}

func (h *npmHandler) rewriteTarballCachePath(rule NPMRewriteRule, pkg, filename string) string {
	return filepath.Join(h.cfg.Cache.Disk.Path, h.rewriteTarballCacheKey(rule, pkg, filename))
}

func (h *npmHandler) rewriteTarballCacheKey(rule NPMRewriteRule, pkg, filename string) string {
	if !h.artifactCacheEnabled() {
		return ""
	}
	hostKey := strings.TrimSpace(rule.TargetHost)
	hostKey = strings.ReplaceAll(hostKey, ":", "_")
	groupKey := strings.ReplaceAll(normalizeTargetGroup(rule.TargetGroup), "/", "__")
	return filepath.ToSlash(filepath.Join("npm-rewrite", hostKey, groupKey, npmCachePackageKey(pkg), filename))
}

func (h *npmHandler) readCacheBytes(key string) ([]byte, time.Time, bool, error) {
	if h.npmCache == nil || key == "" {
		return nil, time.Time{}, false, nil
	}
	body, modifiedAt, err := h.npmCache.Get(key)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, time.Time{}, false, nil
		}
		return nil, time.Time{}, false, err
	}
	return body, modifiedAt, true, nil
}

func (h *npmHandler) writeCacheBytes(key string, body []byte) error {
	if h.npmCache == nil || key == "" {
		return nil
	}
	return h.npmCache.Put(key, body)
}

func (h *npmHandler) deleteCacheKey(key string) error {
	if h.npmCache == nil || key == "" {
		return nil
	}
	return h.npmCache.Delete(key)
}

func (h *npmHandler) readMetadataCache(pkg string) (metadataCacheEntry, bool) {
	if !h.metadataCacheEnabled() {
		return metadataCacheEntry{}, false
	}
	body, modifiedAt, ok, err := h.readCacheBytes(h.metadataCacheKey(pkg))
	if err != nil {
		h.logger.Error("failed to read npm metadata cache body; falling back to upstream fetch", "key", h.metadataCacheKey(pkg), "error", err)
		recordProtocolCacheError("npm", "metadata")
		return metadataCacheEntry{}, false
	}
	if !ok {
		return metadataCacheEntry{}, false
	}
	etagBytes, _, _, err := h.readCacheBytes(h.metadataETagKey(pkg))
	if err != nil {
		h.logger.Error("failed to read npm metadata etag cache body; continuing without etag", "key", h.metadataETagKey(pkg), "error", err)
		recordProtocolCacheError("npm", "metadata")
		etagBytes = nil
	}
	expiresAt := modifiedAt.Add(h.cfg.Protocols.NPM.MetadataTTL)
	if cachedExpiry, ok := h.readMetadataExpiry(pkg); ok {
		expiresAt = cachedExpiry
	}
	return metadataCacheEntry{
		Body:      body,
		ETag:      string(etagBytes),
		ExpiresAt: expiresAt,
		Fresh:     h.now().Before(expiresAt),
	}, true
}

func (h *npmHandler) writeMetadataCache(pkg string, body []byte, etag string) (bool, error) {
	if !h.metadataCacheEnabled() {
		return false, nil
	}
	if err := h.writeCacheBytes(h.metadataCacheKey(pkg), body); err != nil {
		h.logger.Error("failed to write npm metadata cache body; serving uncached body", "key", h.metadataCacheKey(pkg), "error", err)
		return false, err
	}
	if etag != "" {
		if err := h.writeCacheBytes(h.metadataETagKey(pkg), []byte(etag)); err != nil {
			h.logger.Error("failed to write npm metadata etag cache body; serving uncached body", "key", h.metadataETagKey(pkg), "error", err)
			return false, err
		}
	} else if err := h.deleteCacheKey(h.metadataETagKey(pkg)); err != nil {
		h.logger.Error("failed to delete npm metadata etag cache body; serving uncached body", "key", h.metadataETagKey(pkg), "error", err)
		return false, err
	}
	if err := h.writeMetadataExpiry(pkg, h.now().Add(h.cfg.Protocols.NPM.MetadataTTL)); err != nil {
		h.logger.Error("failed to write npm metadata expiry cache body; serving uncached body", "key", h.metadataExpiryKey(pkg), "error", err)
		return false, err
	}
	return true, nil
}

func (h *npmHandler) readMetadataExpiry(pkg string) (time.Time, bool) {
	b, _, ok, err := h.readCacheBytes(h.metadataExpiryKey(pkg))
	if err != nil {
		h.logger.Error("failed to read npm metadata expiry cache body; treating cache entry as stale", "key", h.metadataExpiryKey(pkg), "error", err)
		recordProtocolCacheError("npm", "metadata")
		return time.Time{}, false
	}
	if !ok {
		return time.Time{}, false
	}
	ns, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}, false
	}
	return ns, true
}

func (h *npmHandler) writeMetadataExpiry(pkg string, expiresAt time.Time) error {
	return h.writeCacheBytes(h.metadataExpiryKey(pkg), []byte(expiresAt.UTC().Format(time.RFC3339Nano)))
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
		refreshedExpiry := h.now().Add(h.cfg.Protocols.NPM.MetadataTTL)
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
	return metadataCacheEntry{Body: body, ETag: resp.Header.Get("ETag"), ExpiresAt: h.now().Add(h.cfg.Protocols.NPM.MetadataTTL), Fresh: true}, http.StatusOK, nil
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
