package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubAuthenticator struct {
	skip      bool
	hasAccess bool
	err       error
}

func (s stubAuthenticator) Authenticate(token string, req AuthRequest) (bool, bool, error) {
	return s.skip, s.hasAccess, s.err
}

func TestAuthorizeTokenRejectsSkippedProtectedNPMRequest(t *testing.T) {
	rr := httptest.NewRecorder()
	auths := map[string]Authenticator{
		"gitlab": stubAuthenticator{skip: true, hasAccess: false},
	}

	ok := authorizeToken(rr, auths, "gitlab", "secret", "gitlab", AuthRequest{
		Protocol: "npm",
		Path:     "/@toru/pkg",
		Resource: "@toru/pkg",
	})
	if ok {
		t.Fatalf("authorizeToken() unexpectedly allowed skipped protected npm request")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestAuthorizeTokenAllowsSkippedUnprotectedGoRequest(t *testing.T) {
	rr := httptest.NewRecorder()
	auths := map[string]Authenticator{
		"gitlab": stubAuthenticator{skip: true, hasAccess: false},
	}

	ok := authorizeToken(rr, auths, "gitlab", "secret", "gitlab", AuthRequest{
		Protocol: "go",
		Path:     "/public.example.com/team/workflows/@v/list",
		Resource: "/public.example.com/team/workflows/@v/list",
	})
	if !ok {
		t.Fatalf("authorizeToken() unexpectedly rejected skipped unprotected go request")
	}
}
