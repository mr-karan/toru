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
