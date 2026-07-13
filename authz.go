package main

import (
	"fmt"
	"net/http"
	"strings"
)

func buildAuthenticators(cfg *Config) (map[string]Authenticator, error) {
	authenticators := make(map[string]Authenticator)
	if !cfg.Auth.Enabled {
		return authenticators, nil
	}
	for _, module := range cfg.Auth.Modules {
		auth, err := NewAuthenticator(module)
		if err != nil {
			return nil, fmt.Errorf("failed to create authenticator: %w", err)
		}
		authenticators[module.Name] = auth
	}
	return authenticators, nil
}

func authorizeRequest(w http.ResponseWriter, r *http.Request, auths map[string]Authenticator, moduleName string, req AuthRequest) bool {
	_, _, ok := authorizeRequestToken(r, moduleName)
	if ok {
		return authorizeRequestWithToken(w, auths, moduleName, req, r)
	}

	http.Error(w, "No username or password provided", http.StatusUnauthorized)
	return false
}

func authorizeRequestWithToken(w http.ResponseWriter, auths map[string]Authenticator, moduleName string, req AuthRequest, r *http.Request) bool {
	authMethod, token, ok := authorizeRequestToken(r, moduleName)
	if !ok {
		http.Error(w, "No username or password provided", http.StatusUnauthorized)
		return false
	}
	return authorizeToken(w, auths, authMethod, token, moduleName, req)
}

func authorizeRequestToken(r *http.Request, moduleName string) (string, string, bool) {
	authMethod, password, ok := r.BasicAuth()
	if ok {
		return authMethod, password, true
	}
	if authHeader := strings.TrimSpace(r.Header.Get("Authorization")); authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			token := strings.TrimSpace(parts[1])
			if token != "" {
				return moduleName, token, true
			}
		}
	}
	return "", "", false
}

func authorizeToken(w http.ResponseWriter, auths map[string]Authenticator, authMethod, password, moduleName string, req AuthRequest) bool {
	if authMethod != moduleName {
		http.Error(w, "Invalid auth method", http.StatusBadRequest)
		return false
	}
	auth, ok := auths[authMethod]
	if !ok {
		http.Error(w, "Invalid auth method", http.StatusBadRequest)
		return false
	}
	skip, hasAccess, err := auth.Authenticate(password, req)
	if err != nil {
		if err == ErrorAuthFailed {
			http.Error(w, "Unauthorized", http.StatusForbidden)
			return false
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return false
	}
	if req.Protocol == "npm" && skip {
		http.Error(w, "Unauthorized", http.StatusForbidden)
		return false
	}
	if !hasAccess && !skip {
		http.Error(w, "Unauthorized", http.StatusForbidden)
		return false
	}
	return true
}

func matchProtectedScope(rules []ProtectedScopeRule, pkg string) (ProtectedScopeRule, bool) {
	for _, rule := range rules {
		if pkg == rule.Scope || strings.HasPrefix(pkg, rule.Scope+"/") {
			return rule, true
		}
	}
	return ProtectedScopeRule{}, false
}
