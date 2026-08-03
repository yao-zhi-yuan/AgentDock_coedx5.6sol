//go:build integration

package sandbox

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func repositoryDigest(t *testing.T, repository string) string {
	t.Helper()
	command := exec.Command("git", "ls-files", "-z")
	command.Dir = repository
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	paths := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	sort.Strings(paths)
	hasher := sha256.New()
	for _, path := range paths {
		if path == "" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(repository, filepath.FromSlash(path)))
		if err != nil {
			if info, lstatErr := os.Lstat(filepath.Join(repository, filepath.FromSlash(path))); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
				target, readlinkErr := os.Readlink(filepath.Join(repository, filepath.FromSlash(path)))
				if readlinkErr != nil {
					t.Fatal(readlinkErr)
				}
				content = []byte("symlink:" + target)
			} else {
				t.Fatal(err)
			}
		}
		_, _ = hasher.Write([]byte(path))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write(content)
		_, _ = hasher.Write([]byte{0})
	}
	return fmt.Sprintf("sha256:%x", hasher.Sum(nil))
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
