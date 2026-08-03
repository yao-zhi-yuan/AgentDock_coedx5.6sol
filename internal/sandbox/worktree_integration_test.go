//go:build integration

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitWorktreeProviderIsAttemptScopedAndDestroyLeavesOriginUnchanged(t *testing.T) {
	repository := newFixtureRepository(t)
	beforeDigest := repositoryDigest(t, repository)
	beforeStatus := gitTestOutput(t, repository, "status", "--porcelain=v1")
	provider := NewGitWorktreeProvider(filepath.Join(t.TempDir(), "worktrees"))

	first, err := provider.Create(context.Background(), Spec{
		RunID:      "run-a",
		AttemptID:  "attempt-1",
		Repository: repository,
		Revision:   "HEAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.Create(context.Background(), Spec{
		RunID:      "run-a",
		AttemptID:  "attempt-2",
		Repository: repository,
		Revision:   "HEAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Workspace() == second.Workspace() {
		t.Fatal("attempts must receive independent worktrees")
	}
	if err := os.WriteFile(filepath.Join(first.Workspace(), "main.go"), []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadFile(t, filepath.Join(second.Workspace(), "main.go"))); !strings.Contains(got, "package main") {
		t.Fatalf("second worktree observed first mutation: %q", got)
	}
	originalPointer, err := hideWorktreeGitPointer(first.Workspace())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadFile(t, filepath.Join(first.Workspace(), ".git"))); got != sanitizedGitPointer {
		t.Fatalf("sanitized Git pointer = %q", got)
	}
	if err := restoreWorktreeGitPointer(first.Workspace(), originalPointer); err != nil {
		t.Fatal(err)
	}
	if err := first.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := second.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, workspace := range []string{first.Workspace(), second.Workspace()} {
		if _, err := os.Stat(workspace); !os.IsNotExist(err) {
			t.Fatalf("worktree still exists after Destroy: %s (%v)", workspace, err)
		}
	}
	if after := repositoryDigest(t, repository); after != beforeDigest {
		t.Fatalf("origin digest changed: before=%s after=%s", beforeDigest, after)
	}
	if after := gitTestOutput(t, repository, "status", "--porcelain=v1"); after != beforeStatus {
		t.Fatalf("origin Git status changed: before=%q after=%q", beforeStatus, after)
	}
}

func TestGitWorktreeProviderDoesNotExecuteHostHookOrSmudgeFilter(t *testing.T) {
	repository := newFixtureRepository(t)
	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	filterMarker := filepath.Join(t.TempDir(), "filter-ran")
	hook := filepath.Join(repository, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch \""+hookMarker+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "config", "filter.agentdock-smudge.smudge", "touch \""+filterMarker+"\"; cat")
	if err := os.WriteFile(
		filepath.Join(repository, ".gitattributes"),
		[]byte("filtered.txt filter=agentdock-smudge\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "filtered.txt"), []byte("raw blob\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", ".gitattributes", "filtered.txt")
	runGit(t, repository, "-c", "user.name=AgentDock", "-c", "user.email=agentdock@example.test", "commit", "-m", "filter fixture")

	provider := NewGitWorktreeProvider(filepath.Join(t.TempDir(), "worktrees"))
	instance, err := provider.Create(context.Background(), Spec{
		RunID: "host-safety", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Destroy(context.Background())
	if got := string(mustReadFile(t, filepath.Join(instance.Workspace(), "filtered.txt"))); got != "raw blob\n" {
		t.Fatalf("materialized filtered blob = %q", got)
	}
	for _, marker := range []string{hookMarker, filterMarker} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("untrusted Git hook/filter executed on host: marker=%s err=%v", marker, err)
		}
	}
}

func TestGitWorktreeProviderRejectsCanonicalRootInsideOrigin(t *testing.T) {
	repository := newFixtureRepository(t)
	link := filepath.Join(t.TempDir(), "linked-root")
	if err := os.Symlink(repository, link); err != nil {
		t.Fatal(err)
	}
	provider := NewGitWorktreeProvider(link)
	_, err := provider.Create(context.Background(), Spec{
		RunID: "root-boundary", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	})
	if err == nil {
		t.Fatal("symlink worktree root into origin was accepted")
	}
}

func TestGitWorktreeProviderRejectsCaseAliasRootInsideOrigin(t *testing.T) {
	repository := newFixtureRepository(t)
	aliasRepository := filepath.Join(
		filepath.Dir(repository),
		strings.ToUpper(filepath.Base(repository)),
	)
	repositoryInfo, err := os.Stat(repository)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(aliasRepository)
	if err != nil || !os.SameFile(repositoryInfo, aliasInfo) {
		t.Skip("filesystem is case-sensitive")
	}
	root := filepath.Join(aliasRepository, "case-alias-worktrees")
	provider := NewGitWorktreeProvider(root)
	instance, createErr := provider.Create(context.Background(), Spec{
		RunID: "case-root", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	})
	if createErr == nil {
		_ = instance.Destroy(context.Background())
		t.Fatal("case-aliased worktree root inside origin was accepted")
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("provider modified origin through case alias: %v", statErr)
	}
}

func TestGitWorktreeProviderRejectsAndDoesNotRewritePublicRootPermissions(t *testing.T) {
	repository := newFixtureRepository(t)
	root := filepath.Join(t.TempDir(), "public-worktree-root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := NewGitWorktreeProvider(root)
	_, err := provider.Create(context.Background(), Spec{
		RunID: "root-mode", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	})
	if err == nil {
		t.Fatal("public existing worktree root was accepted")
	}
	info, statErr := os.Stat(root)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("provider rewrote caller-owned root permissions to %04o", got)
	}
}

func TestGitWorktreeProviderCleansPartialMaterializationAndRejectsReservedCase(t *testing.T) {
	repository := newFixtureRepository(t)
	if err := os.MkdirAll(filepath.Join(repository, ".AgentDock"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".AgentDock", "poison"), []byte("deny"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", ".AgentDock/poison")
	runGit(t, repository, "-c", "user.name=AgentDock", "-c", "user.email=agentdock@example.test", "commit", "-m", "reserved path")
	root := filepath.Join(t.TempDir(), "worktrees")
	provider := NewGitWorktreeProvider(root)
	_, err := provider.Create(context.Background(), Spec{
		RunID: "partial", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	})
	if err == nil {
		t.Fatal("case-aliased reserved runtime path was accepted")
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("partial worktree remains after materialization failure: %v", entries)
	}
	list := gitTestOutput(t, repository, "worktree", "list", "--porcelain")
	if strings.Count(list, "worktree ") != 1 {
		t.Fatalf("partial worktree remains registered:\n%s", list)
	}
}

func TestGitWorktreeProviderIgnoresReplaceRefs(t *testing.T) {
	repository := newFixtureRepository(t)
	original := strings.TrimSpace(gitTestOutput(t, repository, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repository, "main.go"), []byte("package replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "main.go")
	runGit(t, repository, "-c", "user.name=AgentDock", "-c", "user.email=agentdock@example.test", "commit", "-m", "replacement")
	replacement := strings.TrimSpace(gitTestOutput(t, repository, "rev-parse", "HEAD"))
	runGit(t, repository, "replace", original, replacement)

	provider := NewGitWorktreeProvider(filepath.Join(t.TempDir(), "worktrees"))
	instance, err := provider.Create(context.Background(), Spec{
		RunID: "replace", AttemptID: "attempt-1", Repository: repository, Revision: original,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Destroy(context.Background())
	if got := string(mustReadFile(t, filepath.Join(instance.Workspace(), "main.go"))); !strings.Contains(got, "package main") {
		t.Fatalf("replace ref changed materialized snapshot: %q", got)
	}
}

func TestGitWorktreeProviderDoesNotLazyFetchMissingPromisorObject(t *testing.T) {
	repository := newFixtureRepository(t)
	blobID := strings.TrimSpace(gitTestOutput(t, repository, "rev-parse", "HEAD:main.go"))
	objectPath := filepath.Join(repository, ".git", "objects", blobID[:2], blobID[2:])
	if err := os.Remove(objectPath); err != nil {
		t.Fatalf("remove promised fixture blob: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "lazy-fetch-helper-ran")
	helper := filepath.Join(t.TempDir(), "promisor-helper")
	if err := os.WriteFile(
		helper,
		[]byte("#!/bin/sh\ntouch \""+marker+"\"\nexit 1\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "config", "core.repositoryformatversion", "1")
	runGit(t, repository, "config", "extensions.partialClone", "origin")
	runGit(t, repository, "config", "remote.origin.promisor", "true")
	runGit(t, repository, "config", "remote.origin.partialclonefilter", "blob:none")
	runGit(t, repository, "config", "remote.origin.url", "ext::"+helper)
	runGit(t, repository, "config", "protocol.ext.allow", "always")

	unsafe := exec.Command("git", "cat-file", "blob", blobID)
	unsafe.Dir = repository
	unsafe.Env = append(os.Environ(), "GIT_PROTOCOL_FROM_USER=1", "GIT_NO_LAZY_FETCH=0")
	_ = unsafe.Run()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("lazy-fetch fixture did not execute its controlled helper: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "worktrees")
	provider := NewGitWorktreeProvider(root)
	_, err := provider.Create(context.Background(), Spec{
		RunID: "lazy-fetch", AttemptID: "attempt-1", Repository: repository, Revision: "HEAD",
	})
	if err == nil {
		t.Fatal("materialization unexpectedly succeeded with a missing promised blob")
	}
	if _, markerErr := os.Stat(marker); !os.IsNotExist(markerErr) {
		t.Fatalf("safe Git materialization executed lazy-fetch helper: %v", markerErr)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed promisor materialization left worktrees: %v", entries)
	}
}

func newFixtureRepository(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_TRACE2_EVENT", "0")
	repository := filepath.Join(t.TempDir(), "origin")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "go.mod"), []byte("module example.test/fixture\n\ngo 1.25.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestFixture(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init")
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "user.name=AgentDock", "-c", "user.email=agentdock@example.test", "commit", "-m", "fixture")
	return repository
}

func runGit(t *testing.T, repository string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = repository
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}

func gitTestOutput(t *testing.T, repository string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = repository
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}
