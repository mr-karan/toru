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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/goproxy/goproxy"
)

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
	return output.Body, nil
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
