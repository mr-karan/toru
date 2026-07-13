package main

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// AuthRequest carries protocol-aware authorization context.
type AuthRequest struct {
	Protocol string
	Path     string
	Resource string
	Scope    string
	RepoPath string
}

// Authenticator defines protocol-aware authentication.
type Authenticator interface {
	Authenticate(token string, req AuthRequest) (bool, bool, error)
}

var (
	_               = Authenticator(&GitLabAuthenticator{})
	_               = Authenticator(&StaticTokenAuthenticator{})
	ErrorAuthFailed = errors.New("authentication failed")
)

// GitLabAuthenticator checks whether a token can access a GitLab project path.
type GitLabAuthenticator struct {
	RootURL       string
	ProtectedURI  string
	ProjectPrefix string
}

func (g *GitLabAuthenticator) Authenticate(token string, req AuthRequest) (bool, bool, error) {
	var (
		projectCandidates []string
		skip              bool
		err               error
	)

	switch req.Protocol {
	case "go":
		projectCandidates, skip, err = extractProjectCandidates(req.Path, g.ProtectedURI)
	case "npm":
		if req.RepoPath != "" {
			projectCandidates, skip, err = extractExplicitRepoPathCandidate(req.RepoPath)
		} else {
			protectedScope := req.Scope
			if protectedScope == "" {
				protectedScope = g.ProtectedURI
			}
			projectCandidates, skip, err = extractNPMProjectCandidates(req.Resource, protectedScope, g.ProjectPrefix)
		}
	default:
		return true, false, nil
	}
	if err != nil || skip {
		return skip, false, err
	}

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

func extractExplicitRepoPathCandidate(repoPath string) ([]string, bool, error) {
	repoPath = strings.Trim(strings.TrimSpace(repoPath), "/")
	if repoPath == "" {
		return nil, false, fmt.Errorf("repo path is empty")
	}
	parts := strings.Split(repoPath, "/")
	if len(parts) < 2 {
		return nil, false, fmt.Errorf("invalid repo path")
	}
	for _, part := range parts {
		if part == "" {
			return nil, false, fmt.Errorf("invalid repo path")
		}
	}
	return []string{repoPath}, false, nil
}

func extractNPMProjectCandidates(pkg, protectedScope, projectPrefix string) ([]string, bool, error) {
	protectedScope = strings.TrimSpace(protectedScope)
	if protectedScope == "" {
		return nil, false, fmt.Errorf("protected_uri is empty")
	}
	if pkg != protectedScope && !strings.HasPrefix(pkg, protectedScope+"/") {
		return nil, true, nil
	}
	name := strings.TrimPrefix(pkg, protectedScope)
	name = strings.TrimPrefix(name, "/")
	if name == "" || strings.Contains(name, "/") {
		return nil, false, fmt.Errorf("invalid npm package path")
	}
	candidate := name
	if projectPrefix != "" {
		candidate = strings.TrimSuffix(projectPrefix, "/") + "/" + name
	}
	return []string{candidate}, false, nil
}

// StaticTokenAuthenticator allows deterministic auth testing and simple deployments.
type StaticTokenAuthenticator struct {
	Token string
}

func (s *StaticTokenAuthenticator) Authenticate(token string, req AuthRequest) (bool, bool, error) {
	if s.Token == "" {
		return false, false, fmt.Errorf("missing token")
	}
	if token != s.Token {
		return false, false, ErrorAuthFailed
	}
	return false, true, nil
}

func NewGitlabAuthenticator(opts map[string]interface{}) (*GitLabAuthenticator, error) {
	url, ok := opts["root_url"].(string)
	if !ok {
		return nil, fmt.Errorf("missing root_url")
	}
	protectedURI, _ := opts["protected_uri"].(string)
	projectPrefix, _ := opts["project_prefix"].(string)
	return &GitLabAuthenticator{RootURL: url, ProtectedURI: protectedURI, ProjectPrefix: projectPrefix}, nil
}

func NewStaticTokenAuthenticator(opts map[string]interface{}) (*StaticTokenAuthenticator, error) {
	token, ok := opts["token"].(string)
	if !ok || token == "" {
		return nil, fmt.Errorf("missing token")
	}
	return &StaticTokenAuthenticator{Token: token}, nil
}

func NewAuthenticator(module AuthModule) (Authenticator, error) {
	switch module.Type {
	case "gitlab_access_token":
		return NewGitlabAuthenticator(module.Options)
	case "static_token":
		return NewStaticTokenAuthenticator(module.Options)
	default:
		return nil, fmt.Errorf("unsupported auth type: %s", module.Type)
	}
}
