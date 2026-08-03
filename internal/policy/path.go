package policy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// NormalizePath rejects lexical traversal and checks every existing path
// prefix for symlink escape. The sandbox helper repeats the operation through
// os.Root immediately before I/O, which supplies the race-resistant open
// boundary on supported Unix platforms.
func NormalizePath(root, candidate string) (string, error) {
	logical, err := normalizeLogicalPath(candidate)
	if err != nil {
		return "", err
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", errors.Join(ErrUnsafePath, err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return "", errors.Join(ErrUnsafePath, err)
	}

	current := canonicalRoot
	parts := strings.Split(filepath.FromSlash(logical), string(filepath.Separator))
	for _, part := range parts {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		_, lstatErr := os.Lstat(current)
		if os.IsNotExist(lstatErr) {
			break
		}
		if lstatErr != nil {
			return "", errors.Join(ErrUnsafePath, lstatErr)
		}
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr != nil {
			return "", errors.Join(ErrUnsafePath, resolveErr)
		}
		if !withinRoot(canonicalRoot, resolved) {
			return "", ErrUnsafePath
		}
		current = resolved
	}
	return logical, nil
}

func withinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
