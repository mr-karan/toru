package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/xanzy/go-gitlab"
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
	var (
		skip      = false
		hasAccess = false
	)

	// Trim the leading slash from the URI
	uri = strings.TrimPrefix(uri, "/")

	// Check if the URI is protected.
	if !strings.HasPrefix(uri, g.ProtectedURI) {
		skip = true
		return skip, hasAccess, nil
	}

	// Remove the ProtectedURI prefix and extract the project path before '/@'
	path := strings.TrimPrefix(uri, g.ProtectedURI)
	path = strings.Split(path, "/@")[0]
	path = strings.TrimPrefix(path, "/")

	// Split the path into components to get namespace and project name
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return skip, hasAccess, fmt.Errorf("invalid project path")
	}

	// Construct the project path with namespace
	pathWithNamespace := strings.Join(parts[:2], "/") // assuming the first two parts are namespace and project name
	if pathWithNamespace == "" {
		return skip, hasAccess, fmt.Errorf("project path is empty")
	}

	// Create a new GitLab client with the user's token
	gl, err := gitlab.NewClient(token, gitlab.WithBaseURL(g.RootURL))
	if err != nil {
		return skip, hasAccess, fmt.Errorf("failed to create GitLab client: %w", err)
	}

	// Get the project details
	prj, resp, err := gl.Projects.GetProject(pathWithNamespace, nil, nil)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return skip, hasAccess, nil
		}

		return skip, hasAccess, fmt.Errorf("failed to get project: %w", err)
	}

	if prj != nil {
		hasAccess = true
	}

	return skip, hasAccess, nil
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
