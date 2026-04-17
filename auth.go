package main

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// Authentiactor interface defines the methods to authenticate the user.
type Authenticator interface {
	Authenticate(token, projectPath string) (bool, bool, error)
}

var (
	_               = Authenticator(&GitLabAuthenticator{})
	ErrorAuthFailed = errors.New("authentication failed")
)

// GitLabAuthenticator is a struct that implements the Authenticator interface.
type GitLabAuthenticator struct {
	RootURL      string
	ProtectedURI string
}

// Authenticate method authenticates the user based on the token and project path.
func (g *GitLabAuthenticator) Authenticate(token, uri string) (bool, bool, error) {
	projectCandidates, skip, err := extractProjectCandidates(uri, g.ProtectedURI)
	if err != nil || skip {
		return skip, false, err
	}

	// Create a new GitLab client with the user's token
	gl, err := gitlab.NewClient(token, gitlab.WithBaseURL(g.RootURL))
	if err != nil {
		return skip, false, fmt.Errorf("failed to create GitLab client: %w", err)
	}

	for _, projectPath := range projectCandidates {
		prj, resp, err := gl.Projects.GetProject(projectPath, nil)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				continue
			}
			if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
				return skip, false, ErrorAuthFailed
			}
			if strings.Contains(err.Error(), "401 Unauthorized") || strings.Contains(err.Error(), "403 Forbidden") {
				return skip, false, ErrorAuthFailed
			}

			return skip, false, fmt.Errorf("failed to get project: %w", err)
		}

		if prj != nil {
			return skip, true, nil
		}
	}

	return skip, false, nil
}

func extractProjectCandidates(uri, protectedURI string) ([]string, bool, error) {
	trimmedURI := strings.TrimPrefix(uri, "/")
	protectedURI = strings.Trim(strings.TrimPrefix(protectedURI, "/"), "/")
	if protectedURI == "" {
		return nil, false, fmt.Errorf("protected_uri is empty")
	}
	if trimmedURI != protectedURI && !strings.HasPrefix(trimmedURI, protectedURI+"/") {
		return nil, true, nil
	}

	projectPath := strings.TrimPrefix(trimmedURI, protectedURI)
	projectPath = strings.Split(projectPath, "/@")[0]
	projectPath = strings.TrimPrefix(projectPath, "/")
	if projectPath == "" {
		return nil, false, fmt.Errorf("project path is empty")
	}

	parts := strings.Split(projectPath, "/")
	if len(parts) < 2 {
		return nil, false, fmt.Errorf("invalid project path")
	}

	projectCandidates := make([]string, 0, len(parts)-1)
	for i := len(parts); i >= 2; i-- {
		projectCandidates = append(projectCandidates, strings.Join(parts[:i], "/"))
	}

	return slices.Compact(projectCandidates), false, nil
}

// NewGitlabAuthenticator creates a new GitLab authenticator.
func NewGitlabAuthenticator(opts map[string]interface{}) (*GitLabAuthenticator, error) {
	// Check if the URL is provided.
	url, ok := opts["root_url"].(string)
	if !ok {
		return nil, fmt.Errorf("missing root_url")
	}

	// Check if the protected URI is provided.
	protectedURIs, ok := opts["protected_uri"].(string)
	if !ok {
		return nil, fmt.Errorf("missing protected_uri")
	}

	return &GitLabAuthenticator{RootURL: url, ProtectedURI: protectedURIs}, nil
}

// NewAuthenticator creates a new authenticator based on the module type.
func NewAuthenticator(module AuthModule) (Authenticator, error) {
	switch module.Type {
	case "gitlab_access_token":
		return NewGitlabAuthenticator(module.Options)
	default:
		return nil, fmt.Errorf("unsupported auth type: %s", module.Type)
	}
}
