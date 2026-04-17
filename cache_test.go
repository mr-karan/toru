package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestIsMutableMetadataTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target string
		want   bool
	}{
		{name: "version list", target: "go.example.com/team/workflows/@v/list", want: true},
		{name: "latest query", target: "go.example.com/team/workflows/@latest", want: true},
		{name: "canonical version info", target: "go.example.com/team/workflows/@v/v0.8.20.info", want: false},
		{name: "canonical pseudo version info", target: "go.example.com/team/workflows/@v/v0.0.0-20260416100000-abcdef123456.info", want: false},
		{name: "major prefix query", target: "go.example.com/team/workflows/@v/v0.info", want: true},
		{name: "branch query", target: "go.example.com/team/workflows/@v/main.info", want: true},
		{name: "v2 canonical version info", target: "go.example.com/libs/toru/v2/@v/v2.1.0.info", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isMutableMetadataTarget(tt.target); got != tt.want {
				t.Fatalf("isMutableMetadataTarget(%q) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
}

func TestMetadataCacherBypassesMutableMetadataWhenTTLDisabled(t *testing.T) {
	t.Parallel()

	base := &fakeCacher{}
	cacher := &metadataCacher{
		cacher:             base,
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		mutableMetadataTTL: 0,
		now:                time.Now,
	}

	if err := cacher.Put(context.Background(), "go.example.com/team/workflows/@v/list", strings.NewReader("v0.8.20")); err != nil {
		t.Fatalf("Put returned error: %v", err)
	}
	if base.putCalls != 0 {
		t.Fatalf("expected mutable metadata Put to be bypassed, got %d calls", base.putCalls)
	}

	_, err := cacher.Get(context.Background(), "go.example.com/team/workflows/@v/list")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Get returned %v, want fs.ErrNotExist", err)
	}
	if base.getCalls != 0 {
		t.Fatalf("expected mutable metadata Get to be bypassed, got %d calls", base.getCalls)
	}
}

func TestMetadataCacherExpiresMutableMetadata(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 16, 12, 0, 0, 0, time.UTC)
	base := &fakeCacher{
		reader: &fakeCacheReadCloser{
			ReadCloser:   io.NopCloser(strings.NewReader("v0.8.20")),
			lastModified: now.Add(-2 * time.Minute),
		},
	}
	cacher := &metadataCacher{
		cacher:             base,
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		mutableMetadataTTL: time.Minute,
		now:                func() time.Time { return now },
	}

	_, err := cacher.Get(context.Background(), "go.example.com/team/workflows/@v/list")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Get returned %v, want fs.ErrNotExist", err)
	}
	if base.getCalls != 1 {
		t.Fatalf("expected underlying Get to be called once, got %d", base.getCalls)
	}
}

func TestMetadataCacherPassesThroughImmutableContent(t *testing.T) {
	t.Parallel()

	base := &fakeCacher{
		reader: &fakeCacheReadCloser{ReadCloser: io.NopCloser(strings.NewReader("{}"))},
	}
	cacher := &metadataCacher{
		cacher:             base,
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		mutableMetadataTTL: 0,
		now:                time.Now,
	}

	rc, err := cacher.Get(context.Background(), "go.example.com/team/workflows/@v/v0.8.20.info")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	rc.Close()

	if base.getCalls != 1 {
		t.Fatalf("expected immutable content Get to hit underlying cache, got %d calls", base.getCalls)
	}
}

type fakeCacher struct {
	reader   io.ReadCloser
	getErr   error
	putErr   error
	getCalls int
	putCalls int
}

func (fc *fakeCacher) Get(context.Context, string) (io.ReadCloser, error) {
	fc.getCalls++
	if fc.getErr != nil {
		return nil, fc.getErr
	}
	if fc.reader == nil {
		return nil, fs.ErrNotExist
	}
	return fc.reader, nil
}

func (fc *fakeCacher) Put(context.Context, string, io.ReadSeeker) error {
	fc.putCalls++
	return fc.putErr
}

type fakeCacheReadCloser struct {
	io.ReadCloser
	lastModified time.Time
}

func (fcrc *fakeCacheReadCloser) LastModified() time.Time {
	return fcrc.lastModified
}
