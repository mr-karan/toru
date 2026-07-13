package main

import (
	"reflect"
	"testing"
)

func TestExtractProjectCandidates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		uri          string
		protectedURI string
		want         []string
		wantSkip     bool
		wantErr      bool
	}{
		{
			name:         "simple project",
			uri:          "/go.example.com/team/workflows/@v/list",
			protectedURI: "go.example.com",
			want:         []string{"team/workflows"},
		},
		{
			name:         "subgroup and subdirectory module",
			uri:          "/go.example.com/org/suborg/service/subpkg/@v/v1.2.3.mod",
			protectedURI: "go.example.com",
			want: []string{
				"org/suborg/service/subpkg",
				"org/suborg/service",
				"org/suborg",
			},
		},
		{
			name:         "unprotected path",
			uri:          "/public.example.com/team/workflows/@v/list",
			protectedURI: "go.example.com",
			wantSkip:     true,
		},
		{
			name:         "prefix collision is not protected",
			uri:          "/go.example.com.evil/team/workflows/@v/list",
			protectedURI: "go.example.com",
			wantSkip:     true,
		},
		{
			name:         "invalid short path",
			uri:          "/go.example.com/workflows/@v/list",
			protectedURI: "go.example.com",
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, skip, err := extractProjectCandidates(tt.uri, tt.protectedURI)
			if (err != nil) != tt.wantErr {
				t.Fatalf("extractProjectCandidates() error = %v, wantErr %v", err, tt.wantErr)
			}
			if skip != tt.wantSkip {
				t.Fatalf("extractProjectCandidates() skip = %v, want %v", skip, tt.wantSkip)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("extractProjectCandidates() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractNPMProjectCandidates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		pkg            string
		protectedScope string
		projectPrefix  string
		want           []string
		wantSkip       bool
		wantErr        bool
	}{
		{
			name:           "scoped package with prefix",
			pkg:            "@toru/fixture-scoped",
			protectedScope: "@toru",
			projectPrefix:  "team/npm",
			want:           []string{"team/npm/fixture-scoped"},
		},
		{
			name:           "unprotected scope skipped",
			pkg:            "@other/fixture",
			protectedScope: "@toru",
			wantSkip:       true,
		},
		{
			name:           "invalid nested npm path",
			pkg:            "@toru/fixture/subpath",
			protectedScope: "@toru",
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, skip, err := extractNPMProjectCandidates(tt.pkg, tt.protectedScope, tt.projectPrefix)
			if (err != nil) != tt.wantErr {
				t.Fatalf("extractNPMProjectCandidates() error = %v, wantErr %v", err, tt.wantErr)
			}
			if skip != tt.wantSkip {
				t.Fatalf("extractNPMProjectCandidates() skip = %v, want %v", skip, tt.wantSkip)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("extractNPMProjectCandidates() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStaticTokenAuthenticator(t *testing.T) {
	t.Parallel()
	a := &StaticTokenAuthenticator{Token: "secret"}

	if _, ok, err := a.Authenticate("wrong", AuthRequest{Protocol: "npm", Resource: "@toru/fixture"}); err != ErrorAuthFailed || ok {
		t.Fatalf("wrong token must fail auth, got ok=%v err=%v", ok, err)
	}
	if skip, ok, err := a.Authenticate("secret", AuthRequest{Protocol: "npm", Resource: "@toru/fixture"}); err != nil || skip || !ok {
		t.Fatalf("correct token must pass auth, got skip=%v ok=%v err=%v", skip, ok, err)
	}
}

func TestGitLabAuthenticatorPrefersExplicitRepoPathForNPM(t *testing.T) {
	t.Parallel()
	got, skip, err := extractExplicitRepoPathCandidate("platform/commons/foo")
	if err != nil || skip || len(got) != 1 || got[0] != "platform/commons/foo" {
		t.Fatalf("extractExplicitRepoPathCandidate() = %v skip=%v err=%v, want [platform/commons/foo] false nil", got, skip, err)
	}
}

func TestExtractExplicitRepoPathCandidate(t *testing.T) {
	t.Parallel()
	candidates, skip, err := extractExplicitRepoPathCandidate("platform/commons/foo")
	if err != nil {
		t.Fatalf("extractExplicitRepoPathCandidate() error = %v", err)
	}
	if skip {
		t.Fatalf("extractExplicitRepoPathCandidate() skip = true, want false")
	}
	if len(candidates) != 1 || candidates[0] != "platform/commons/foo" {
		t.Fatalf("extractExplicitRepoPathCandidate() = %v, want [platform/commons/foo]", candidates)
	}
}

func TestNewGitlabAuthenticatorAllowsNPMOnlyConfig(t *testing.T) {
	t.Parallel()
	a, err := NewGitlabAuthenticator(map[string]interface{}{
		"root_url":       "https://gitlab.example.com",
		"project_prefix": "team/npm",
	})
	if err != nil {
		t.Fatalf("NewGitlabAuthenticator() error = %v", err)
	}
	if a.ProtectedURI != "" {
		t.Fatalf("ProtectedURI = %q, want empty", a.ProtectedURI)
	}
	if a.ProjectPrefix != "team/npm" {
		t.Fatalf("ProjectPrefix = %q, want %q", a.ProjectPrefix, "team/npm")
	}
}
