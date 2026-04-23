package main

import (
	"strings"
	"testing"
)

func TestBuildFetcherEnvIncludesVanityAndTargetPaths(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.RewriteRules = []struct {
		VanityPath string `koanf:"vanity_path"`
		TargetPath string `koanf:"target_path"`
	}{
		{VanityPath: "go.example.com", TargetPath: "gitlab.example.com"},
		{VanityPath: "go.other.example.com", TargetPath: "scm.example.com"},
	}

	patterns := buildPrivatePatterns(cfg)
	env := buildFetcherEnv(cfg, patterns)
	joined := strings.Join(env, "\n")

	for _, expected := range []string{
		"GOPROXY=https://proxy.golang.org,direct",
		"GOPRIVATE=gitlab.example.com,go.example.com,go.other.example.com,scm.example.com",
		"GONOPROXY=gitlab.example.com,go.example.com,go.other.example.com,scm.example.com",
		"GONOSUMDB=gitlab.example.com,go.example.com,go.other.example.com,scm.example.com",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("buildFetcherEnv() missing %q\nfull env:\n%s", expected, joined)
		}
	}
}

func TestShouldUseIsolatedGoCache(t *testing.T) {
	t.Parallel()

	f := &fetcher{patterns: "gitlab.example.com,go.example.com"}

	tests := []struct {
		path string
		want bool
	}{
		{path: "go.example.com/team/workflows", want: true},
		{path: "gitlab.example.com/team/workflows", want: true},
		{path: "rsc.io/quote", want: false},
	}

	for _, tt := range tests {
		if got := f.shouldUseIsolatedGoCache(tt.path); got != tt.want {
			t.Fatalf("shouldUseIsolatedGoCache(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
