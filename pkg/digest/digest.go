package digest

import (
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Digest computes a hash of all content under the specified directory path.
// It recursively walks the directory tree, including file contents and paths in the hash.
// Symlinks are resolved to their real paths before being included in the digest.
//
// The digest is deterministic - the same directory structure and content will always
// produce the same hash, regardless of filesystem timestamps or other metadata.
//
// The hash.Hash parameter allows the caller to choose the hash algorithm (e.g., SHA256, MD5).
// Returns an error if the directory cannot be read.
func Digest(path string, h hash.Hash) error {

	// Collect all file paths first so we can sort them for deterministic ordering
	var paths []string
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Get relative path for consistent hashing
		relPath, err := filepath.Rel(path, p)
		if err != nil {
			return fmt.Errorf("failed to get relative path for %s: %w", p, err)
		}

		// Skip the root directory itself
		if relPath == "." {
			return nil
		}

		paths = append(paths, p)
		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to walk directory %s: %w", path, err)
	}

	// Sort paths for deterministic ordering
	sort.Strings(paths)

	// Process each path in sorted order
	for _, p := range paths {
		relPath, err := filepath.Rel(path, p)
		if err != nil {
			return fmt.Errorf("failed to get relative path for %s: %w", p, err)
		}

		// Get file info, following symlinks
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("failed to stat %s: %w", p, err)
		}

		// Write the relative path to the hash for determinism
		if _, err := h.Write([]byte(relPath)); err != nil {
			return fmt.Errorf("failed to write path to hash: %w", err)
		}

		// If it's a directory, just include the path
		if info.IsDir() {
			continue
		}

		// For files, include the content
		if err := hashFile(h, p); err != nil {
			return fmt.Errorf("failed to hash file %s: %w", p, err)
		}
	}

	return nil
}

// hashFile reads a file and writes its content to the hash
func hashFile(h io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	return nil
}
