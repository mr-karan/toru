package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/goproxy/goproxy"
)

type fetcher struct {
	upstream *goproxy.GoFetcher
	cfg      *Config
	logger   *slog.Logger
}

func newFetcher(cfg *Config, logger *slog.Logger) (*fetcher, error) {
	vanityPaths := make([]string, len(cfg.RewriteRules))
	for i, rule := range cfg.RewriteRules {
		vanityPaths[i] = rule.VanityPath
	}
	vanityPathsStr := strings.Join(vanityPaths, ",")

	return &fetcher{
		upstream: &goproxy.GoFetcher{
			Env: append(os.Environ(),
				"GOPROXY=https://proxy.golang.org,direct",
				fmt.Sprintf("GOPRIVATE=%s", vanityPathsStr),
				fmt.Sprintf("GONOPROXY=%s", vanityPathsStr),
			),
		},
		cfg:    cfg,
		logger: logger,
	}, nil
}

func (f *fetcher) rewrite(path string) string {
	for _, rule := range f.cfg.RewriteRules {
		if strings.HasPrefix(path, rule.VanityPath) {
			f.logger.Debug("Rewriting path",
				"original", path,
				"vanity", rule.VanityPath,
				"target", rule.TargetPath,
			)
			return strings.Replace(path, rule.VanityPath, rule.TargetPath, 1)
		}
	}
	return path
}

func (f *fetcher) Query(ctx context.Context, path, query string) (version string, t time.Time, err error) {
	startTime := time.Now()
	defer func() {
		upstreamFetchDuration.UpdateDuration(startTime)
		if err != nil {
			errorsTotal.Inc()
		}
	}()

	rewrittenPath := f.rewrite(path)
	if rewrittenPath != path {
		rewriteRulesApplied.Inc()
	}
	return f.upstream.Query(ctx, rewrittenPath, query)
}

func (f *fetcher) List(ctx context.Context, path string) (versions []string, err error) {
	return f.upstream.List(ctx, f.rewrite(path))
}

func (f *fetcher) Download(ctx context.Context, path, version string) (info, mod, zip io.ReadSeekCloser, err error) {
	startTime := time.Now()
	defer func() {
		upstreamFetchDuration.UpdateDuration(startTime)
		if err != nil {
			errorsTotal.Inc()
		}
	}()

	rewrittenPath := f.rewrite(path)
	if rewrittenPath != path {
		rewriteRulesApplied.Inc()
	}
	info, mod, originalZip, err := f.upstream.Download(ctx, rewrittenPath, version)
	if err != nil {
		return nil, nil, nil, err
	}

	// Only rewrite if there are rewrite rules and the rewritten path is not the same as the original path
	if len(f.cfg.RewriteRules) > 0 && rewrittenPath != path {
		f.logger.Debug("Rewriting zip", "original", path, "rewritten", rewrittenPath)
		rewrittenZip, err := f.rewriteZip(originalZip)
		if err != nil {
			return nil, nil, nil, err
		}
		return info, mod, rewrittenZip, nil
	}

	return info, mod, originalZip, nil
}

func (f *fetcher) rewriteZip(originalZip io.ReadSeekCloser) (io.ReadSeekCloser, error) {
	// Seek to the beginning of the file.
	if _, err := originalZip.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	// Create a ReaderAt from the ReadSeeker
	readerAt := &readerAtFromReadSeeker{originalZip}

	// Get the size of the zip file so that we can create a new zip reader.
	size, err := originalZip.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	// Reset the seek to the beginning of the file.
	// This is done because the zip reader expects to be at the beginning of the file.
	if _, err := originalZip.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	// Open the zip reader
	reader, err := zip.NewReader(readerAt, size)
	if err != nil {
		return nil, err
	}

	// Create a buffer to store the new zip file
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)

	for _, file := range reader.File {
		// Rewrite the file path
		newName := file.Name
		for _, rule := range f.cfg.RewriteRules {
			if strings.Contains(newName, rule.TargetPath) {
				newName = strings.Replace(newName, rule.TargetPath, rule.VanityPath, 1)
				break
			}
		}

		// Create a new file in the zip archive
		newFile, err := writer.Create(newName)
		if err != nil {
			return nil, err
		}

		// Open the original file
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}

		// Copy the file contents
		if _, err = io.Copy(newFile, rc); err != nil {
			rc.Close()
			return nil, err
		}
		rc.Close()
	}

	if err = writer.Close(); err != nil {
		return nil, err
	}

	// Create a ReadSeekCloser from the buffer
	return &readSeekCloser{bytes.NewReader(buf.Bytes())}, nil
}

// readerAtFromReadSeeker adapts a ReadSeeker to a ReaderAt
type readerAtFromReadSeeker struct {
	io.ReadSeeker
}

func (r *readerAtFromReadSeeker) ReadAt(p []byte, off int64) (n int, err error) {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return r.Read(p)
}

// readSeekCloser wraps a *bytes.Reader to implement io.ReadSeekCloser
type readSeekCloser struct {
	*bytes.Reader
}

func (r *readSeekCloser) Close() error {
	return nil // No-op, as *bytes.Reader doesn't need closing
}
