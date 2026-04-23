package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/goproxy/goproxy"
	"golang.org/x/mod/module"
)

type metadataCacher struct {
	cacher             goproxy.Cacher
	logger             *slog.Logger
	mutableMetadataTTL time.Duration
	now                func() time.Time
}

func newMetadataCacher(cacher goproxy.Cacher, ttl time.Duration, logger *slog.Logger) goproxy.Cacher {
	return &metadataCacher{
		cacher:             cacher,
		logger:             logger,
		mutableMetadataTTL: ttl,
		now:                time.Now,
	}
}

func (mc *metadataCacher) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if !isMutableMetadataTarget(name) {
		return mc.cacher.Get(ctx, name)
	}

	if mc.mutableMetadataTTL <= 0 {
		mc.logger.Debug("bypassing persistent cache for mutable metadata", "package", name)
		cacheMisses.Inc()
		return nil, fs.ErrNotExist
	}

	rc, err := mc.cacher.Get(ctx, name)
	if err != nil {
		return nil, err
	}

	modTime, ok := cacheModTime(rc)
	if !ok {
		rc.Close()
		mc.logger.Warn("mutable metadata cache entry missing mod time; treating as stale", "package", name)
		cacheMisses.Inc()
		return nil, fs.ErrNotExist
	}

	if mc.now().After(modTime.Add(mc.mutableMetadataTTL)) {
		rc.Close()
		mc.logger.Debug("mutable metadata cache entry expired", "package", name, "age", mc.now().Sub(modTime), "ttl", mc.mutableMetadataTTL)
		cacheMisses.Inc()
		return nil, fs.ErrNotExist
	}

	return rc, nil
}

func (mc *metadataCacher) Put(ctx context.Context, name string, content io.ReadSeeker) error {
	if isMutableMetadataTarget(name) && mc.mutableMetadataTTL <= 0 {
		mc.logger.Debug("skipping persistent cache write for mutable metadata", "package", name)
		return nil
	}

	return mc.cacher.Put(ctx, name, content)
}

type cacheObject struct {
	io.ReadCloser
	size         int64
	lastModified time.Time
	etag         string
}

func (co *cacheObject) Size() int64 {
	return co.size
}

func (co *cacheObject) LastModified() time.Time {
	return co.lastModified
}

func (co *cacheObject) ETag() string {
	return co.etag
}

func cacheModTime(rc io.ReadCloser) (time.Time, bool) {
	type lastModified interface{ LastModified() time.Time }
	type modTime interface{ ModTime() time.Time }

	if lm, ok := rc.(lastModified); ok {
		if t := lm.LastModified(); !t.IsZero() {
			return t, true
		}
	}
	if mt, ok := rc.(modTime); ok {
		if t := mt.ModTime(); !t.IsZero() {
			return t, true
		}
	}

	return time.Time{}, false
}

func isMutableMetadataTarget(name string) bool {
	if strings.HasSuffix(name, "/@latest") || strings.HasSuffix(name, "/@v/list") {
		return true
	}
	if !strings.HasSuffix(name, ".info") {
		return false
	}

	modulePath, versionWithExt, ok := strings.Cut(name, "/@v/")
	if !ok {
		return false
	}

	version := strings.TrimSuffix(versionWithExt, ".info")
	if version == versionWithExt {
		return false
	}

	unescapedModulePath, err := module.UnescapePath(modulePath)
	if err != nil {
		return true
	}
	unescapedVersion, err := module.UnescapeVersion(version)
	if err != nil {
		return true
	}

	return !isCanonicalModuleVersion(unescapedModulePath, unescapedVersion)
}

func isCanonicalModuleVersion(modulePath, version string) bool {
	if version == "" || version != module.CanonicalVersion(version) {
		return false
	}

	_, pathMajor, ok := module.SplitPathVersion(modulePath)
	if !ok {
		return false
	}

	return module.CheckPathMajor(version, pathMajor) == nil
}

type s3Cacher struct {
	client *s3.Client
	bucket string
	logger *slog.Logger
}

func newS3Cacher(cfg *Config, logger *slog.Logger) (goproxy.Cacher, error) {
	ctx := context.Background()
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(cfg.Cache.S3.Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.Cache.S3.AccessKey,
			cfg.Cache.S3.SecretKey,
			"",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg)

	return &s3Cacher{
		client: client,
		bucket: cfg.Cache.S3.Bucket,
		logger: logger,
	}, nil
}

func (s3c *s3Cacher) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	s3c.logger.Debug("cache get called", "cache_type", "s3", "package", name)
	output, err := s3c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s3c.bucket),
		Key:    aws.String(name),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if strings.Contains(err.Error(), "NoSuchKey") || errors.As(err, &nsk) {
			s3c.logger.Debug("cache miss", "cache_type", "s3", "package", name)
			cacheMisses.Inc()
			return nil, fs.ErrNotExist
		}
		s3c.logger.Debug("cache error", "cache_type", "s3", "package", name, "error", err)
		cacheErrors.Inc()
		return nil, err
	}

	s3c.logger.Debug("cache hit", "cache_type", "s3", "package", name)
	cacheHits.Inc()

	var lastModified time.Time
	if output.LastModified != nil {
		lastModified = *output.LastModified
	}

	var etag string
	if output.ETag != nil {
		etag = *output.ETag
	}

	var size int64
	if output.ContentLength != nil {
		size = *output.ContentLength
	}

	return &cacheObject{
		ReadCloser:   output.Body,
		size:         size,
		lastModified: lastModified,
		etag:         etag,
	}, nil
}

func (s3c *s3Cacher) Put(ctx context.Context, name string, content io.ReadSeeker) error {
	s3c.logger.Debug("cache put called", "cache_type", "s3", "package", name)
	size, err := content.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return err
	}

	contentType := "application/octet-stream"
	nameExt := filepath.Ext(name)
	switch {
	case nameExt == ".info", strings.HasSuffix(name, "/@latest"):
		contentType = "application/json; charset=utf-8"
	case nameExt == ".mod", strings.HasSuffix(name, "/@v/list"):
		contentType = "text/plain; charset=utf-8"
	case nameExt == ".zip":
		contentType = "application/zip"
	case strings.HasPrefix(name, "sumdb/"):
		if elems := strings.Split(name, "/"); len(elems) >= 3 {
			switch elems[2] {
			case "latest", "lookup":
				contentType = "text/plain; charset=utf-8"
			}
		}
	}

	_, err = s3c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s3c.bucket),
		Key:           aws.String(name),
		Body:          content,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		s3c.logger.Debug("cache write error", "cache_type", "s3", "package", name, "error", err)
		cacheErrors.Inc()
	} else {
		s3c.logger.Debug("cache write", "cache_type", "s3", "package", name)
		cacheWrites.Inc()
	}
	return err
}

// diskCacher is a wrapper around goproxy.DirCacher that adds debug logging
type diskCacher struct {
	cacher goproxy.Cacher
	logger *slog.Logger
}

func newDiskCacher(path string, logger *slog.Logger) goproxy.Cacher {
	return &diskCacher{
		cacher: goproxy.DirCacher(path),
		logger: logger,
	}
}

func (dc *diskCacher) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	dc.logger.Debug("cache get called", "cache_type", "disk", "package", name)
	rc, err := dc.cacher.Get(ctx, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			dc.logger.Debug("cache miss", "cache_type", "disk", "package", name)
			cacheMisses.Inc()
		} else {
			dc.logger.Debug("cache error", "cache_type", "disk", "package", name, "error", err)
			cacheErrors.Inc()
		}
		return nil, err
	}
	dc.logger.Debug("cache hit", "cache_type", "disk", "package", name)
	cacheHits.Inc()
	return rc, nil
}

func (dc *diskCacher) Put(ctx context.Context, name string, content io.ReadSeeker) error {
	dc.logger.Debug("cache put called", "cache_type", "disk", "package", name)
	err := dc.cacher.Put(ctx, name, content)
	if err != nil {
		dc.logger.Debug("cache write error", "cache_type", "disk", "package", name, "error", err)
		cacheErrors.Inc()
	} else {
		dc.logger.Debug("cache write", "cache_type", "disk", "package", name)
		cacheWrites.Inc()
	}
	return err
}
