package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"golang.org/x/mod/semver"
)

type gitLabTag struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}

type gitLabPackageVersion struct {
	Version  string
	Tag      string
	Manifest map[string]any
}

func (h *npmHandler) synthesizeRewrittenMetadata(r *http.Request, pkg string, rule NPMRewriteRule) ([]byte, int, error) {
	repoPath, err := repoPathForPackage(rule, pkg)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	_, gitlabToken, ok := authorizeRequestToken(r, rule.AuthModule)
	if !ok {
		return nil, http.StatusUnauthorized, fmt.Errorf("no username or password provided")
	}
	versions, err := h.fetchGitLabVersions(r, pkg, rule, repoPath, gitlabToken)
	if err != nil {
		var httpErr gitLabHTTPError
		if ok := errorAs(err, &httpErr); ok {
			return nil, httpErr.StatusCode, fmt.Errorf("%s", httpErr.Message)
		}
		return nil, http.StatusBadGateway, err
	}
	if len(versions) == 0 {
		return nil, http.StatusNotFound, fmt.Errorf("no valid semver tags found for %s", repoPath)
	}
	return synthesizeNPMRewriteMetadata(pkg, h.cfg.Protocols.NPM.BaseURL, versions)
}

func (h *npmHandler) fetchGitLabVersions(r *http.Request, pkg string, rule NPMRewriteRule, repoPath, gitlabToken string) ([]gitLabPackageVersion, error) {
	tags, err := h.fetchGitLabTags(r, rule, repoPath, gitlabToken)
	if err != nil {
		return nil, err
	}
	versionToTag := map[string]string{}
	versionToTarget := map[string]string{}
	orderedVersions := make([]string, 0, len(tags))
	for _, tag := range tags {
		version, ok := normalizeGitTagVersion(tag.Name)
		if !ok {
			continue
		}
		if existingTag, ok := versionToTag[version]; ok {
			existingTarget := versionToTarget[version]
			candidateTarget := tag.Target
			if existingTarget == "" {
				existingTarget = existingTag
			}
			if candidateTarget == "" {
				candidateTarget = tag.Name
			}
			if existingTarget != candidateTarget {
				return nil, fmt.Errorf("ambiguous tags for version %s: %s, %s", version, existingTag, tag.Name)
			}
			versionToTag[version] = preferredGitTag(existingTag, tag.Name, version)
			continue
		}
		orderedVersions = append(orderedVersions, version)
		versionToTag[version] = tag.Name
		if tag.Target != "" {
			versionToTarget[version] = tag.Target
		}
	}
	slices.SortFunc(orderedVersions, func(a, b string) int {
		if semver.Compare(a, b) > 0 {
			return -1
		}
		if semver.Compare(a, b) < 0 {
			return 1
		}
		return 0
	})
	versions := make([]gitLabPackageVersion, 0, len(orderedVersions))
	for _, version := range orderedVersions {
		manifest, err := h.fetchGitLabManifest(r, rule, repoPath, versionToTag[version], gitlabToken)
		if err != nil {
			return nil, err
		}
		name, _ := manifest["name"].(string)
		if name != pkg {
			return nil, fmt.Errorf("package.json name mismatch for %s at %s: got %q want %q", repoPath, versionToTag[version], name, pkg)
		}
		versions = append(versions, gitLabPackageVersion{
			Version:  strings.TrimPrefix(version, "v"),
			Tag:      versionToTag[version],
			Manifest: manifest,
		})
	}
	return versions, nil
}

func (h *npmHandler) fetchGitLabTags(r *http.Request, rule NPMRewriteRule, repoPath, gitlabToken string) ([]gitLabTag, error) {
	projectPath := url.PathEscape(repoPath)
	endpoint := h.gitLabRuleBaseURL(rule) + "/api/v4/projects/" + projectPath + "/repository/tags"
	resp, body, err := h.doAuthorizedUpstreamGet(r, endpoint, gitlabToken)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, gitLabHTTPError{StatusCode: mapGitLabStatus(resp.StatusCode), Message: string(body)}
	}
	var tags []gitLabTag
	if err := json.Unmarshal(body, &tags); err != nil {
		return nil, err
	}
	return tags, nil
}

func (h *npmHandler) fetchGitLabManifest(r *http.Request, rule NPMRewriteRule, repoPath, tag, gitlabToken string) (map[string]any, error) {
	projectPath := url.PathEscape(repoPath)
	endpoint := h.gitLabRuleBaseURL(rule) + "/api/v4/projects/" + projectPath + "/repository/archive.tar.gz?sha=" + url.QueryEscape(tag)
	resp, body, err := h.doAuthorizedUpstreamGet(r, endpoint, gitlabToken)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, gitLabHTTPError{StatusCode: mapGitLabStatus(resp.StatusCode), Message: string(body)}
	}
	return extractRootPackageJSON(body)
}

func extractRootPackageJSON(body []byte) (map[string]any, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		clean := path.Clean(hdr.Name)
		parts := strings.Split(clean, "/")
		if len(parts) == 2 && parts[1] == "package.json" {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			var manifest map[string]any
			if err := json.Unmarshal(data, &manifest); err != nil {
				return nil, err
			}
			return manifest, nil
		}
	}
	return nil, fmt.Errorf("root package.json not found in archive")
}

func preferredGitTag(existing, candidate, version string) string {
	exact := strings.TrimPrefix(version, "v")
	if candidate == exact {
		return candidate
	}
	if existing == exact {
		return existing
	}
	return existing
}

func normalizeGitTagVersion(tag string) (string, bool) {
	if tag == "" {
		return "", false
	}
	version := tag
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	if !semver.IsValid(version) {
		return "", false
	}
	return version, true
}

func (h *npmHandler) gitLabRuleBaseURL(rule NPMRewriteRule) string {
	host := strings.TrimSpace(rule.TargetHost)
	if strings.HasPrefix(host, "127.0.0.1:") || strings.HasPrefix(host, "localhost:") {
		return "http://" + host
	}
	return "https://" + host
}

type gitLabHTTPError struct {
	StatusCode int
	Message    string
}

func (e gitLabHTTPError) Error() string { return e.Message }

func mapGitLabStatus(status int) int {
	switch status {
	case http.StatusNotFound:
		return http.StatusNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return http.StatusForbidden
	default:
		return http.StatusBadGateway
	}
}

func errorAs(err error, target *gitLabHTTPError) bool {
	httpErr, ok := err.(gitLabHTTPError)
	if !ok {
		return false
	}
	*target = httpErr
	return true
}
