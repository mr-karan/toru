package main

import (
	"fmt"
	"log"
	"strings"

	"github.com/xanzy/go-gitlab"
)

// Authentiactor interface defines the methods to authenticate the user.
type Authenticator interface {
	Authenticate(token, projectPath string) (bool, bool, error)
}

var (
	_ = Authenticator(&GitLabAuthenticator{})
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

	// Check if the project path is protected.
	// If the project path is not protected, skip the authentication.
	// Trim the leading slash
	uri = uri[1:]

	// Check if the URI is protected.
	if !strings.HasPrefix(uri, g.ProtectedURI) {
		skip = true
		return skip, hasAccess, nil
	}

	// Check if URI contains `/@`.
	if !strings.Contains(uri, "/@") {
		return skip, hasAccess, fmt.Errorf("invalid URI")
	}

	// Remove the protectedURI from the URI. And extract till `/@`.
	// This will give us the project path.
	path := strings.TrimPrefix(uri, g.ProtectedURI)
	path = strings.Split(path, "/@")[0]
	path = strings.TrimPrefix(path, "/")

	log.Println("extracted path", path)

	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return skip, hasAccess, fmt.Errorf("invalid project path")
	}

	// Check if the project path is empty.
	pathWithNamespace := parts[0] + "/" + parts[1]
	if pathWithNamespace == "" {
		return skip, hasAccess, fmt.Errorf("project path is empty")
	}

	gl, err := gitlab.NewClient(token, gitlab.WithBaseURL(g.RootURL))
	if err != nil {
		return skip, hasAccess, fmt.Errorf("failed to create GitLab client: %w", err)
	}

	isSimple := true
	prj, _, err := gl.Projects.ListProjects(&gitlab.ListProjectsOptions{
		Simple: &isSimple,
	})
	if err != nil {
		return skip, hasAccess, fmt.Errorf("failed to list projects: %w", err)
	}

	if len(prj) == 0 || prj == nil {
		return skip, hasAccess, fmt.Errorf("projects not found")
	}

	for _, p := range prj {
		if p.PathWithNamespace == pathWithNamespace {
			skip = false
			hasAccess = true
			return skip, hasAccess, nil
		}
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
