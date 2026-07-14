package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"path"
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

func rewrittenTarballVersion(pkg, filename string) (string, error) {
	expectedPrefix := strings.TrimSuffix(rewrittenTarballFilename(pkg, "0"), "0.tgz")
	if !strings.HasPrefix(filename, expectedPrefix) || !strings.HasSuffix(filename, ".tgz") {
		return "", fmt.Errorf("filename %q does not match rewritten package %q", filename, pkg)
	}
	version := strings.TrimSuffix(strings.TrimPrefix(filename, expectedPrefix), ".tgz")
	if version == "" {
		return "", fmt.Errorf("missing version in tarball filename %q", filename)
	}
	return version, nil
}

func synthesizeNPMTarballFromGitLabArchive(pkg string, body []byte) ([]byte, error) {
	gzReader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer gzReader.Close()

	tr := tar.NewReader(gzReader)
	var out bytes.Buffer
	gzWriter := gzip.NewWriter(&out)
	tw := tar.NewWriter(gzWriter)
	foundPackageJSON := false

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
		if len(parts) < 2 {
			continue
		}
		relPath := path.Join(parts[1:]...)
		if relPath == "." || relPath == "" {
			continue
		}
		newHdr := *hdr
		newHdr.Name = path.Join("package", relPath)
		if relPath == "package.json" && (hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA) {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			var manifest map[string]any
			if err := json.Unmarshal(data, &manifest); err != nil {
				return nil, err
			}
			name, _ := manifest["name"].(string)
			if name != pkg {
				return nil, fmt.Errorf("package.json name mismatch in tarball source: got %q want %q", name, pkg)
			}
			foundPackageJSON = true
			newHdr.Size = int64(len(data))
			if err := tw.WriteHeader(&newHdr); err != nil {
				return nil, err
			}
			if _, err := tw.Write(data); err != nil {
				return nil, err
			}
			continue
		}
		if err := tw.WriteHeader(&newHdr); err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			if _, err := io.Copy(tw, tr); err != nil {
				return nil, err
			}
		}
	}
	if !foundPackageJSON {
		return nil, fmt.Errorf("root package.json not found in archive")
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gzWriter.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
