package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goproxy/goproxy"
	"golang.org/x/mod/module"
)

type fetcher struct {
	upstream  *goproxy.GoFetcher
	cfg       *Config
	logger    *slog.Logger
	baseEnv   []string
	patterns  string
	transport http.RoundTripper
}

func newFetcher(cfg *Config, logger *slog.Logger, transport http.RoundTripper) (*fetcher, error) {
	patterns := buildPrivatePatterns(cfg)
	baseEnv := buildFetcherEnv(cfg, patterns)

	f := &fetcher{
		cfg:       cfg,
		logger:    logger,
		baseEnv:   baseEnv,
		patterns:  patterns,
		transport: transport,
	}
	f.upstream = f.newGoFetcher(baseEnv)

	return f, nil
}

func buildPrivatePatterns(cfg *Config) string {
	privatePatterns := make([]string, 0, len(cfg.RewriteRules)*2)
	for _, rule := range cfg.RewriteRules {
		privatePatterns = append(privatePatterns, rule.VanityPath, rule.TargetPath)
	}
	slices.Sort(privatePatterns)
	privatePatterns = slices.Compact(privatePatterns)
	return strings.Join(privatePatterns, ",")
}

func buildFetcherEnv(cfg *Config, privatePatternsStr string) []string {
	return append(os.Environ(),
		"GOPROXY=https://proxy.golang.org,direct",
		fmt.Sprintf("GOPRIVATE=%s", privatePatternsStr),
		fmt.Sprintf("GONOPROXY=%s", privatePatternsStr),
		fmt.Sprintf("GONOSUMDB=%s", privatePatternsStr),
	)
}

func (f *fetcher) newGoFetcher(env []string) *goproxy.GoFetcher {
	return &goproxy.GoFetcher{
		Env:       env,
		Transport: f.transport,
	}
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

// withFetchTimeout bounds the upstream fetch/build phase to Server.FetchTimeout.
// It is applied only around upstream operations (Query/List/Download), never the
// whole request: the returned readers from Download are os.File-backed handles
// over the completed module cache, so releasing the deadline once the upstream
// call returns is safe and keeps the deadline off the post-200 client copy.
// A FetchTimeout of 0 disables the bound (matches the config's documented "0").
func (f *fetcher) withFetchTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if f.cfg.Server.FetchTimeout > 0 {
		return context.WithTimeout(ctx, f.cfg.Server.FetchTimeout)
	}
	return ctx, func() {}
}

func (f *fetcher) Query(ctx context.Context, path, query string) (version string, t time.Time, err error) {
	startTime := time.Now()
	defer func() {
		upstreamFetchDuration.UpdateDuration(startTime)
		if err != nil {
			errorsTotal.Inc()
		}
	}()

	ctx, cancel := f.withFetchTimeout(ctx)
	defer cancel()

	rewrittenPath := f.rewrite(path)
	if rewrittenPath != path {
		rewriteRulesApplied.Inc()
	}

	if !f.shouldUseIsolatedGoCache(rewrittenPath) {
		return f.upstream.Query(ctx, rewrittenPath, query)
	}

	return f.queryWithFreshGoEnv(ctx, rewrittenPath, query)
}

func (f *fetcher) List(ctx context.Context, path string) (versions []string, err error) {
	startTime := time.Now()
	defer func() {
		upstreamFetchDuration.UpdateDuration(startTime)
		if err != nil {
			errorsTotal.Inc()
		}
	}()

	ctx, cancel := f.withFetchTimeout(ctx)
	defer cancel()

	rewrittenPath := f.rewrite(path)
	if rewrittenPath != path {
		rewriteRulesApplied.Inc()
	}

	if !f.shouldUseIsolatedGoCache(rewrittenPath) {
		return f.upstream.List(ctx, rewrittenPath)
	}

	return f.listWithFreshGoEnv(ctx, rewrittenPath)
}

func (f *fetcher) shouldUseIsolatedGoCache(path string) bool {
	if f.patterns == "" {
		return false
	}

	return module.MatchPrefixPatterns(f.patterns, path)
}

func (f *fetcher) Download(ctx context.Context, path, version string) (info, mod, zip io.ReadSeekCloser, err error) {
	startTime := time.Now()
	defer func() {
		upstreamFetchDuration.UpdateDuration(startTime)
		if err != nil {
			errorsTotal.Inc()
		}
	}()

	// Bound only the build phase (upstream download + in-memory zip rewrite).
	// The returned readers are materialized over the module cache / an in-memory
	// buffer, so cancelling once this function returns does not affect the
	// subsequent client copy. This keeps a slow cold build from being aborted
	// mid-stream (HTTP/2 INTERNAL_ERROR); instead it errors before headers and
	// goproxy returns a retryable response.
	ctx, cancel := f.withFetchTimeout(ctx)
	defer cancel()

	rewrittenPath := f.rewrite(path)
	if rewrittenPath != path {
		rewriteRulesApplied.Inc()
	}

	info, mod, originalZip, err := f.upstream.Download(ctx, rewrittenPath, version)
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		f.logger.Info("Retrying module download with isolated Go caches",
			"path", rewrittenPath,
			"version", version,
			"error", err,
		)
		info, mod, originalZip, err = f.downloadWithFreshGoEnv(ctx, rewrittenPath, version)
	}
	if err != nil {
		return nil, nil, nil, err
	}

	if len(f.cfg.RewriteRules) > 0 && rewrittenPath != path {
		f.logger.Debug("Rewriting zip", "original", path, "rewritten", rewrittenPath)
		rewrittenZip, err := f.rewriteZip(originalZip)
		if err != nil {
			info.Close()
			mod.Close()
			originalZip.Close()
			return nil, nil, nil, err
		}
		originalZip.Close()
		return info, mod, rewrittenZip, nil
	}

	return info, mod, originalZip, nil
}

func (f *fetcher) queryWithFreshGoEnv(ctx context.Context, path, query string) (string, time.Time, error) {
	upstream, cleanup, err := f.newIsolatedGoFetcher()
	if err != nil {
		return "", time.Time{}, err
	}
	defer cleanup()

	return upstream.Query(ctx, path, query)
}

func (f *fetcher) listWithFreshGoEnv(ctx context.Context, path string) ([]string, error) {
	upstream, cleanup, err := f.newIsolatedGoFetcher()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return upstream.List(ctx, path)
}

func (f *fetcher) downloadWithFreshGoEnv(ctx context.Context, path, version string) (info, mod, zip io.ReadSeekCloser, err error) {
	upstream, cleanup, err := f.newIsolatedGoFetcher()
	if err != nil {
		return nil, nil, nil, err
	}

	info, mod, zip, err = upstream.Download(ctx, path, version)
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}

	info, mod, zip = wrapReadSeekClosersWithCleanup(cleanup, info, mod, zip)
	return info, mod, zip, nil
}

func (f *fetcher) newIsolatedGoFetcher() (*goproxy.GoFetcher, func(), error) {
	tempRoot, err := os.MkdirTemp("", "toru-go-fetch-*")
	if err != nil {
		return nil, nil, err
	}

	env := append([]string{}, f.baseEnv...)
	env = append(env,
		"GOMODCACHE="+filepath.Join(tempRoot, "modcache"),
		"GOCACHE="+filepath.Join(tempRoot, "gocache"),
		"GOPATH="+filepath.Join(tempRoot, "gopath"),
	)

	return f.newGoFetcher(env), func() { _ = os.RemoveAll(tempRoot) }, nil
}

func wrapReadSeekClosersWithCleanup(cleanup func(), readers ...io.ReadSeekCloser) (info, mod, zip io.ReadSeekCloser) {
	var (
		remaining   int32 = int32(len(readers))
		cleanupOnce sync.Once
	)

	wrapped := make([]io.ReadSeekCloser, 0, len(readers))
	for _, reader := range readers {
		reader := reader
		var closeOnce sync.Once
		wrapped = append(wrapped, &readSeekCloserWithClose{
			ReadSeeker: reader,
			closeFn: func() error {
				var closeErr error
				closeOnce.Do(func() {
					closeErr = reader.Close()
					if atomic.AddInt32(&remaining, -1) == 0 {
						cleanupOnce.Do(cleanup)
					}
				})
				return closeErr
			},
		})
	}

	return wrapped[0], wrapped[1], wrapped[2]
}

func (f *fetcher) rewriteZip(originalZip io.ReadSeekCloser) (io.ReadSeekCloser, error) {
	if _, err := originalZip.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	readerAt := &readerAtFromReadSeeker{originalZip}

	size, err := originalZip.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if _, err := originalZip.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	reader, err := zip.NewReader(readerAt, size)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)

	for _, file := range reader.File {
		newName := file.Name
		for _, rule := range f.cfg.RewriteRules {
			if strings.Contains(newName, rule.TargetPath) {
				newName = strings.Replace(newName, rule.TargetPath, rule.VanityPath, 1)
				break
			}
		}

		newFile, err := writer.Create(newName)
		if err != nil {
			return nil, err
		}

		rc, err := file.Open()
		if err != nil {
			return nil, err
		}

		if _, err = io.Copy(newFile, rc); err != nil {
			rc.Close()
			return nil, err
		}
		rc.Close()
	}

	if err = writer.Close(); err != nil {
		return nil, err
	}

	return &readSeekCloser{bytes.NewReader(buf.Bytes())}, nil
}

type readerAtFromReadSeeker struct {
	io.ReadSeeker
}

func (r *readerAtFromReadSeeker) ReadAt(p []byte, off int64) (n int, err error) {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return r.Read(p)
}

type readSeekCloser struct {
	*bytes.Reader
}

func (r *readSeekCloser) Close() error {
	return nil
}

type readSeekCloserWithClose struct {
	io.ReadSeeker
	closeFn func() error
}

func (r *readSeekCloserWithClose) Close() error {
	return r.closeFn()
}
