package main

import (
	"fmt"
	"net"
	"strings"
)

type NPMRewriteRule struct {
	Scope       string `koanf:"scope"`
	TargetHost  string `koanf:"target_host"`
	TargetGroup string `koanf:"target_group"`
	AuthModule  string `koanf:"auth_module"`
}

func matchNPMRewriteRule(rules []NPMRewriteRule, pkg string) (NPMRewriteRule, bool) {
	for _, rule := range rules {
		if pkg == rule.Scope || strings.HasPrefix(pkg, rule.Scope+"/") {
			return rule, true
		}
	}
	return NPMRewriteRule{}, false
}

func repoPathForPackage(rule NPMRewriteRule, pkg string) (string, error) {
	if pkg != rule.Scope && !strings.HasPrefix(pkg, rule.Scope+"/") {
		return "", fmt.Errorf("package %q does not match rewrite scope %q", pkg, rule.Scope)
	}
	name := strings.TrimPrefix(pkg, rule.Scope)
	name = strings.TrimPrefix(name, "/")
	if name == "" || strings.Contains(name, "/") || !isValidPackageSegment(name) {
		return "", fmt.Errorf("invalid rewritten npm package path %q", pkg)
	}
	group := normalizeTargetGroup(rule.TargetGroup)
	if group == "" {
		return "", fmt.Errorf("rewrite rule target_group is empty")
	}
	return group + "/" + name, nil
}

func validateNPMRewriteRules(npmCfg NPMProtocolConfig) error {
	seenScopes := map[string]struct{}{}
	protectedScopes := map[string]struct{}{}
	for _, rule := range npmCfg.ProtectedScopes {
		protectedScopes[strings.TrimSpace(rule.Scope)] = struct{}{}
	}
	for _, rule := range npmCfg.RewriteRules {
		scope := strings.TrimSpace(rule.Scope)
		if !isValidNPMScope(scope) {
			return fmt.Errorf("invalid npm rewrite scope %q", rule.Scope)
		}
		if _, ok := seenScopes[scope]; ok {
			return fmt.Errorf("duplicate npm rewrite scope %q", scope)
		}
		seenScopes[scope] = struct{}{}
		if _, ok := protectedScopes[scope]; ok {
			return fmt.Errorf("npm rewrite scope %q conflicts with protected scope; rewrite rules have their own auth_module", scope)
		}
		if !isValidRewriteTargetHost(rule.TargetHost) {
			return fmt.Errorf("npm rewrite scope %q has invalid target_host %q", scope, rule.TargetHost)
		}
		if strings.TrimSpace(rule.AuthModule) == "" {
			return fmt.Errorf("npm rewrite scope %q must declare auth_module", scope)
		}
		if !isValidTargetGroup(rule.TargetGroup) {
			return fmt.Errorf("npm rewrite scope %q has invalid target_group %q", scope, rule.TargetGroup)
		}
	}
	return nil
}

func isValidNPMScope(scope string) bool {
	if !strings.HasPrefix(scope, "@") {
		return false
	}
	name := strings.TrimPrefix(scope, "@")
	if name == "" || strings.Contains(name, "/") {
		return false
	}
	return isValidPackageSegment(name)
}

func isValidTargetGroup(group string) bool {
	group = normalizeTargetGroup(group)
	if group == "" {
		return false
	}
	parts := strings.Split(group, "/")
	for _, part := range parts {
		if !isValidPackageSegment(part) {
			return false
		}
	}
	return true
}

func normalizeTargetGroup(group string) string {
	return strings.Trim(strings.TrimSpace(group), "/")
}

func isValidPackageSegment(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
		if i == 0 && (r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func isValidRewriteTargetHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, "/?#@") {
		return false
	}
	normalized := normalizeListenerHost(host)
	if normalized == "" {
		return false
	}
	if strings.Contains(host, ":") {
		parsedHost, parsedPort, err := net.SplitHostPort(host)
		if err != nil || parsedHost == "" || parsedPort == "" {
			return false
		}
		normalized = parsedHost
	}
	return normalized != "" && !strings.ContainsAny(normalized, "/?#@")
}
