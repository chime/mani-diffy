package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestDigest(t *testing.T) {
	tests := []struct {
		name  string
		files []file
		want  string
	}{
		{
			name: "basic directory",
			files: []file{
				{name: "file1.txt", content: "content1"},
				{name: "file2.txt", content: "content2"},
			},
			want: "aa048f5c591bf3ebbc02ac20f0c84f2669f22bd043e88a44a57f3f27cda52ae7",
		},
		{
			name: "file names affect hash",
			files: []file{
				{name: "different1.txt", content: "content1"},
				{name: "different2.txt", content: "content2"},
			},
			want: "da5ccfddcdecbe0ff72334d16234759794b1084e6d26dabde16ec826f4b58879",
		},
		{
			name: "with subdirectory",
			files: []file{
				{name: "file1.txt", content: "content1"},
				{name: "subdir/file2.txt", content: "content2"},
				{name: "subdir/file3.txt", content: "content3"},
			},
			want: "41217b30e5a1cd74aa659c4fbcb01fcf0a23f95c65fe0c3fcd7bdd05a3a2fa30",
		},
		{
			name: "with symlink to file",
			files: []file{
				{name: "target.txt", content: "content"},
				{name: "link.txt", symlink: "target.txt"},
			},
			want: "c020ab2266dc1f79afab18ece47828c41bcbab2551955a62039f3fba5fa6f1ff",
		},
		{
			name: "with symlink to directory",
			files: []file{
				{name: "targetdir/file1.txt", content: "content1"},
				{name: "targetdir/file2.txt", content: "content2"},
				{name: "linkdir", symlink: "targetdir"},
			},
			want: "f6cdd79e7c0be70c79a5da677b34a8d3654881f35c6e15ec310867c12abc2b33",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFiles(t, dir, tt.files)
			validateDigest(t, dir, tt.want)
		})
	}
}

type file struct {
	name    string // path of the file
	content string // content for regular files
	symlink string // target for symlinks (if non-empty, this is a symlink)
}

// setupFiles creates files and symlinks in the specified directory.
// Paths can include subdirectories (e.g., "subdir/file.txt") and parent directories
// will be created automatically.
func setupFiles(t *testing.T, dir string, files []file) {
	t.Helper()
	for _, f := range files {
		fullPath := filepath.Join(dir, f.name)
		// Create parent directories if needed
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatal(err)
		}

		if f.symlink != "" {
			// Create a symlink
			target := filepath.Join(dir, f.symlink)
			if err := os.Symlink(target, fullPath); err != nil {
				t.Fatal(err)
			}
		} else {
			// Create a regular file
			if err := os.WriteFile(fullPath, []byte(f.content), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// validateDigest computes the digest of a directory and compares it to the expected hash.
func validateDigest(t *testing.T, dir string, want string) {
	t.Helper()
	h := sha256.New()
	if err := Digest(dir, h); err != nil {
		t.Fatalf("Digest() error = %v", err)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		t.Errorf("Digest() = %s, want %s", got, want)
	}
}
