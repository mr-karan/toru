package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
}

func newS3Cacher(cfg *Config) (goproxy.Cacher, error) {
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
	}, nil
}

func (s3c *s3Cacher) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	output, err := s3c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s3c.bucket),
		Key:    aws.String(name),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if strings.Contains(err.Error(), "NoSuchKey") || errors.As(err, &nsk) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}

	return output.Body, nil
}

func (s3c *s3Cacher) Put(ctx context.Context, name string, content io.ReadSeeker) error {
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
	return err
}
