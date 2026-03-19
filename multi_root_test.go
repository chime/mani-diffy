package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComputeOutputPath(t *testing.T) {
	tests := []struct {
		name      string
		renderDir string
		region    string
		env       string
		want      string
	}{
		{
			name:      "standard us-east-1",
			renderDir: "zz.auto-generated",
			region:    "us-east-1",
			env:       "chalk",
			want:      "zz.auto-generated/root-use1-chalk",
		},
		{
			name:      "us-east-2",
			renderDir: "zz.auto-generated",
			region:    "us-east-2",
			env:       "finplat-dev",
			want:      "zz.auto-generated/root-use2-finplat-dev",
		},
		{
			name:      "nonstable special case",
			renderDir: "zz.auto-generated",
			region:    "us-east-1",
			env:       "nonstable",
			want:      "zz.auto-generated/root-nonstable-a004",
		},
		{
			name:      "nonstable3 is not special",
			renderDir: "zz.auto-generated",
			region:    "us-east-1",
			env:       "nonstable3",
			want:      "zz.auto-generated/root-use1-nonstable3",
		},
		{
			name:      "prod",
			renderDir: "zz.auto-generated",
			region:    "us-east-1",
			env:       "prod",
			want:      "zz.auto-generated/root-use1-prod",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeOutputPath(tt.renderDir, tt.region, tt.env)
			if got != tt.want {
				t.Errorf("computeOutputPath(%q, %q, %q) = %q, want %q",
					tt.renderDir, tt.region, tt.env, got, tt.want)
			}
		})
	}
}

func TestDiscoverEnvironments(t *testing.T) {
	// Create a temp directory structure:
	// rootsDir/us-east-1/chalk/
	// rootsDir/us-east-1/prod/
	// rootsDir/us-east-2/finplat-dev/
	tmpDir := t.TempDir()

	dirs := []string{
		"us-east-1/chalk",
		"us-east-1/prod",
		"us-east-2/finplat-dev",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(tmpDir, d), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Also create a file at the top level that should be ignored
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	envs, err := discoverEnvironments(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(envs) != 3 {
		t.Fatalf("expected 3 environments, got %d", len(envs))
	}

	// Build a set for easier checking
	found := make(map[string]string)
	for _, e := range envs {
		key := e.region + "/" + e.env
		found[key] = e.rootPath
	}

	for _, d := range dirs {
		if _, ok := found[d]; !ok {
			t.Errorf("expected to find environment %s", d)
		}
		expectedPath := filepath.Join(tmpDir, d)
		if found[d] != expectedPath {
			t.Errorf("environment %s: rootPath = %q, want %q", d, found[d], expectedPath)
		}
	}
}

func TestDiscoverEnvironments_NonexistentDir(t *testing.T) {
	_, err := discoverEnvironments("/nonexistent/path")
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}
