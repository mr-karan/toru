package main

import "testing"

func TestMatchNPMRewriteRule(t *testing.T) {
	rule, ok := matchNPMRewriteRule([]NPMRewriteRule{{Scope: "@example-commons"}}, "@example-commons/foo")
	if !ok {
		t.Fatalf("expected rewrite rule match")
	}
	if rule.Scope != "@example-commons" {
		t.Fatalf("matched scope = %q, want %q", rule.Scope, "@example-commons")
	}
}

func TestRepoPathForPackage(t *testing.T) {
	repoPath, err := repoPathForPackage(NPMRewriteRule{
		Scope:       "@example-commons",
		TargetHost:  "gitlab.example.com",
		TargetGroup: "commons",
		AuthModule:  "gitlab",
	}, "@example-commons/foo")
	if err != nil {
		t.Fatalf("repoPathForPackage() error = %v", err)
	}
	if repoPath != "commons/foo" {
		t.Fatalf("repoPathForPackage() = %q, want %q", repoPath, "commons/foo")
	}
}

func TestRepoPathForPackageSupportsSubgroups(t *testing.T) {
	repoPath, err := repoPathForPackage(NPMRewriteRule{
		Scope:       "@example-platform",
		TargetHost:  "gitlab.example.com",
		TargetGroup: "platform/commons",
		AuthModule:  "gitlab",
	}, "@example-platform/foo")
	if err != nil {
		t.Fatalf("repoPathForPackage() error = %v", err)
	}
	if repoPath != "platform/commons/foo" {
		t.Fatalf("repoPathForPackage() = %q, want %q", repoPath, "platform/commons/foo")
	}
}

func TestRepoPathForPackageRejectsNestedPackageName(t *testing.T) {
	_, err := repoPathForPackage(NPMRewriteRule{
		Scope:       "@example-commons",
		TargetHost:  "gitlab.example.com",
		TargetGroup: "commons",
		AuthModule:  "gitlab",
	}, "@example-commons/foo/bar")
	if err == nil {
		t.Fatalf("expected nested package name to be rejected")
	}
}

func TestRepoPathForPackageRejectsDotSegmentName(t *testing.T) {
	for _, pkg := range []string{"@example-commons/..", "@example-commons/.foo"} {
		t.Run(pkg, func(t *testing.T) {
			_, err := repoPathForPackage(NPMRewriteRule{
				Scope:       "@example-commons",
				TargetHost:  "gitlab.example.com",
				TargetGroup: "commons",
				AuthModule:  "gitlab",
			}, pkg)
			if err == nil {
				t.Fatalf("expected invalid rewritten package name %q to be rejected", pkg)
			}
		})
	}
}

func TestValidateNPMRewriteRules(t *testing.T) {
	tests := []struct {
		name    string
		cfg     NPMProtocolConfig
		wantErr bool
	}{
		{
			name: "valid rewrite rule",
			cfg: NPMProtocolConfig{RewriteRules: []NPMRewriteRule{{
				Scope:       "@example-commons",
				TargetHost:  "gitlab.example.com",
				TargetGroup: "commons",
				AuthModule:  "gitlab",
			}}},
		},
		{
			name: "duplicate rewrite scope",
			cfg: NPMProtocolConfig{RewriteRules: []NPMRewriteRule{{
				Scope:       "@example-commons",
				TargetHost:  "gitlab.example.com",
				TargetGroup: "commons",
				AuthModule:  "gitlab",
			}, {
				Scope:       "@example-commons",
				TargetHost:  "gitlab.example.com",
				TargetGroup: "commons-alt",
				AuthModule:  "gitlab",
			}}},
			wantErr: true,
		},
		{
			name: "invalid scope format",
			cfg: NPMProtocolConfig{RewriteRules: []NPMRewriteRule{{
				Scope:       "example-commons",
				TargetHost:  "gitlab.example.com",
				TargetGroup: "commons",
				AuthModule:  "gitlab",
			}}},
			wantErr: true,
		},
		{
			name: "missing auth module",
			cfg: NPMProtocolConfig{RewriteRules: []NPMRewriteRule{{
				Scope:       "@example-commons",
				TargetHost:  "gitlab.example.com",
				TargetGroup: "commons",
			}}},
			wantErr: true,
		},
		{
			name: "invalid target host with userinfo",
			cfg: NPMProtocolConfig{RewriteRules: []NPMRewriteRule{{
				Scope:       "@example-commons",
				TargetHost:  "user@gitlab.example.com",
				TargetGroup: "commons",
				AuthModule:  "gitlab",
			}}},
			wantErr: true,
		},
		{
			name: "invalid target host with query",
			cfg: NPMProtocolConfig{RewriteRules: []NPMRewriteRule{{
				Scope:       "@example-commons",
				TargetHost:  "gitlab.example.com?x=1",
				TargetGroup: "commons",
				AuthModule:  "gitlab",
			}}},
			wantErr: true,
		},
		{
			name: "invalid target group",
			cfg: NPMProtocolConfig{RewriteRules: []NPMRewriteRule{{
				Scope:       "@example-commons",
				TargetHost:  "gitlab.example.com",
				TargetGroup: "../commons",
				AuthModule:  "gitlab",
			}}},
			wantErr: true,
		},
		{
			name: "protected scope conflict",
			cfg: NPMProtocolConfig{
				ProtectedScopes: []ProtectedScopeRule{{Scope: "@example-commons", AuthModule: "gitlab"}},
				RewriteRules: []NPMRewriteRule{{
					Scope:       "@example-commons",
					TargetHost:  "gitlab.example.com",
					TargetGroup: "commons",
					AuthModule:  "gitlab",
				}},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNPMRewriteRules(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateNPMRewriteRules() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
