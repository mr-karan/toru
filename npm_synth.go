package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

var allowedManifestFields = []string{
	"dependencies",
	"peerDependencies",
	"optionalDependencies",
	"bin",
	"engines",
	"type",
	"main",
	"module",
	"types",
	"exports",
}

func synthesizeNPMRewriteMetadata(pkg, baseURL string, versions []gitLabPackageVersion) ([]byte, int, error) {
	if len(versions) == 0 {
		return nil, 0, fmt.Errorf("no versions available")
	}
	doc := map[string]any{
		"name": pkg,
		"dist-tags": map[string]any{
			"latest": versions[0].Version,
		},
		"versions": map[string]any{},
	}
	versionsMap := doc["versions"].(map[string]any)
	for _, version := range versions {
		entry := map[string]any{
			"name":    pkg,
			"version": version.Version,
			"dist": map[string]any{
				"tarball": fmt.Sprintf("%s/%s/-/%s", strings.TrimSuffix(baseURL, "/"), encodeNPMPackagePath(pkg), rewrittenTarballFilename(pkg, version.Version)),
			},
		}
		for _, field := range allowedManifestFields {
			if value, ok := version.Manifest[field]; ok {
				entry[field] = value
			}
		}
		versionsMap[version.Version] = entry
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, 0, err
	}
	return body, 0, nil
}

func rewrittenTarballFilename(pkg, version string) string {
	parts := strings.Split(pkg, "/")
	return parts[len(parts)-1] + "-" + version + ".tgz"
}
