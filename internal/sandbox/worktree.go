package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type GitWorktreeProvider struct {
	root string
}

func NewGitWorktreeProvider(root string) *GitWorktreeProvider {
	return &GitWorktreeProvider{root: root}
}

func (provider *GitWorktreeProvider) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	if spec.RunID == "" ||
		spec.AttemptID == "" ||
		spec.Repository == "" ||
		spec.Revision == "" ||
		strings.HasPrefix(spec.Revision, "-") ||
		strings.ContainsAny(spec.RunID, "\x00\r\n\t") ||
		strings.ContainsAny(spec.AttemptID, "\x00\r\n\t") ||
		strings.ContainsRune(spec.Revision, 0) {
		return nil, errors.New("run_id, attempt_id, repository, and safe revision are required")
	}
	repository, err := filepath.Abs(spec.Repository)
	if err != nil {
		return nil, fmt.Errorf("resolve repository: %w", err)
	}
	topLevel, err := gitOutput(ctx, repository, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	canonicalRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		return nil, fmt.Errorf("canonicalize repository: %w", err)
	}
	canonicalTopLevel, err := filepath.EvalSymlinks(strings.TrimSpace(topLevel))
	if err != nil {
		return nil, fmt.Errorf("canonicalize repository root: %w", err)
	}
	if canonicalRepository != canonicalTopLevel {
		return nil, fmt.Errorf("repository %q is not Git top level %q", canonicalRepository, canonicalTopLevel)
	}

	root := provider.root
	if root == "" {
		root = filepath.Join(os.TempDir(), "agentdock-worktrees")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	prospectiveRoot, err := canonicalProspectivePath(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve prospective worktree root: %w", err)
	}
	if pathWithin(canonicalRepository, prospectiveRoot) {
		return nil, errors.New("worktree root must be outside the source repository")
	}
	if err := os.MkdirAll(absoluteRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create worktree root: %w", err)
	}
	rootInfo, err := os.Lstat(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("inspect worktree root: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, errors.New("worktree root must be a real directory")
	}
	stat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("worktree root must be owned by the current user")
	}
	if rootInfo.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf(
			"worktree root permissions must already be 0700, got %04o",
			rootInfo.Mode().Perm(),
		)
	}
	canonicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("canonicalize worktree root: %w", err)
	}
	if pathWithin(canonicalRepository, canonicalRoot) {
		return nil, errors.New("canonical worktree root must be outside the source repository")
	}
	temporary, err := os.MkdirTemp(absoluteRoot, safeName(spec.RunID)+"-"+safeName(spec.AttemptID)+"-")
	if err != nil {
		return nil, fmt.Errorf("allocate worktree path: %w", err)
	}
	temporaryInfo, err := os.Lstat(temporary)
	if err != nil || !temporaryInfo.IsDir() || temporaryInfo.Mode().Perm() != 0o700 {
		_ = os.Remove(temporary)
		return nil, errors.New("allocated worktree path is not a private directory")
	}
	canonicalTemporary, err := filepath.EvalSymlinks(temporary)
	if err != nil || !pathWithin(canonicalRoot, canonicalTemporary) {
		_ = os.Remove(temporary)
		return nil, errors.New("allocated worktree path escaped the configured root")
	}
	temporary = canonicalTemporary
	if err := os.Remove(temporary); err != nil {
		return nil, fmt.Errorf("prepare worktree path: %w", err)
	}
	revision, err := gitOutput(ctx, canonicalRepository, "rev-parse", "--verify", spec.Revision+"^{commit}")
	if err != nil {
		return nil, err
	}
	revision = strings.TrimSpace(revision)
	if !isObjectID(revision) {
		return nil, fmt.Errorf("revision resolved to invalid object ID %q", revision)
	}
	if _, err := gitOutput(
		ctx,
		canonicalRepository,
		"worktree",
		"add",
		"--no-checkout",
		"--detach",
		temporary,
		revision,
	); err != nil {
		cleanupErr := cleanupKnownWorktree(canonicalRepository, canonicalRoot, temporary)
		return nil, errors.Join(fmt.Errorf("create no-checkout Git worktree: %w", err), cleanupErr)
	}
	if err := materializeGitTree(ctx, canonicalRepository, temporary, revision); err != nil {
		cleanupErr := cleanupKnownWorktree(canonicalRepository, canonicalRoot, temporary)
		return nil, errors.Join(fmt.Errorf("materialize Git tree: %w", err), cleanupErr)
	}
	return &gitWorktree{
		repository: canonicalRepository,
		root:       canonicalRoot,
		workspace:  temporary,
		scope: Scope{
			RunID:     spec.RunID,
			AttemptID: spec.AttemptID,
		},
	}, nil
}

type gitWorktree struct {
	mu         sync.Mutex
	repository string
	root       string
	workspace  string
	scope      Scope
	destroyed  bool
}

func (worktree *gitWorktree) Execute(context.Context, Request) (Result, error) {
	return Result{}, ErrCommandNotAllowed
}

func (worktree *gitWorktree) Workspace() string {
	return worktree.workspace
}

func (worktree *gitWorktree) Scope() Scope {
	return worktree.scope
}

func (worktree *gitWorktree) Destroy(ctx context.Context) error {
	worktree.mu.Lock()
	defer worktree.mu.Unlock()
	if worktree.destroyed {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := cleanupKnownWorktreeContext(
		cleanupCtx,
		worktree.repository,
		worktree.root,
		worktree.workspace,
	); err != nil {
		return err
	}
	worktree.destroyed = true
	return nil
}

func gitOutput(ctx context.Context, repository string, arguments ...string) (string, error) {
	command := safeGitCommand(ctx, repository, arguments...)
	var stdout limitedBuffer
	stdout.limit = 16 << 20
	var stderr limitedBuffer
	stderr.limit = 4096
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(stderr.Bytes())))
	}
	if stdout.truncated {
		return "", fmt.Errorf("git %s output exceeds 16 MiB", strings.Join(arguments, " "))
	}
	return string(stdout.Bytes()), nil
}

func safeGitCommand(ctx context.Context, repository string, arguments ...string) *exec.Cmd {
	safeArguments := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.ext.allow=never",
	}
	safeArguments = append(safeArguments, arguments...)
	command := exec.CommandContext(ctx, "git", safeArguments...)
	command.Dir = repository
	command.Env = []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PROTOCOL_FROM_USER=0",
		"GIT_TERMINAL_PROMPT=0",
		"HOME=/nonexistent",
		"LANG=C",
		"LC_ALL=C",
		"TMPDIR=/tmp",
	}
	return command
}

func canonicalProspectivePath(candidate string) (string, error) {
	current := filepath.Clean(candidate)
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	canonical, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	for index := len(missing) - 1; index >= 0; index-- {
		canonical = filepath.Join(canonical, missing[index])
	}
	return canonical, nil
}

func materializeGitTree(ctx context.Context, repository, workspace, revision string) error {
	tree, err := gitOutput(ctx, repository, "ls-tree", "-r", "-z", "--full-tree", "-l", revision)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, encoded := range strings.Split(tree, "\x00") {
		if encoded == "" {
			continue
		}
		metadata, name, ok := strings.Cut(encoded, "\t")
		if !ok {
			return errors.New("malformed git ls-tree entry")
		}
		fields := strings.Fields(metadata)
		if len(fields) != 4 || fields[1] != "blob" || !isObjectID(fields[2]) {
			if len(fields) >= 2 && fields[1] == "commit" {
				if err := makeMaterializedDirectory(root, name); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("unsupported git tree entry %q", metadata)
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 {
			return fmt.Errorf("invalid git blob size %q", fields[3])
		}
		if err := validateTreePath(name); err != nil {
			return err
		}
		if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
			return fmt.Errorf("create materialized parent for %q: %w", name, err)
		}
		switch fields[0] {
		case "100644", "100755":
			mode := fs.FileMode(0o600)
			if fields[0] == "100755" {
				mode = 0o700
			}
			if err := materializeBlob(ctx, repository, root, name, fields[2], mode); err != nil {
				return err
			}
		case "120000":
			if size > 4096 {
				return fmt.Errorf("symlink target for %q exceeds 4096 bytes", name)
			}
			target, err := gitOutput(ctx, repository, "cat-file", "blob", fields[2])
			if err != nil {
				return err
			}
			if int64(len(target)) != size {
				return fmt.Errorf("symlink target size mismatch for %q", name)
			}
			if err := root.Symlink(target, name); err != nil {
				return fmt.Errorf("materialize symlink %q: %w", name, err)
			}
		default:
			return fmt.Errorf("unsupported git file mode %q for %q", fields[0], name)
		}
	}
	return nil
}

func materializeBlob(
	ctx context.Context,
	repository string,
	root *os.Root,
	name string,
	objectID string,
	mode fs.FileMode,
) error {
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create materialized file %q: %w", name, err)
	}
	command := safeGitCommand(ctx, repository, "cat-file", "blob", objectID)
	command.Stdout = file
	var stderr limitedBuffer
	stderr.limit = 4096
	command.Stderr = &stderr
	runErr := command.Run()
	closeErr := file.Close()
	if runErr != nil {
		_ = root.Remove(name)
		return fmt.Errorf("materialize blob %q: %w: %s", name, runErr, strings.TrimSpace(string(stderr.Bytes())))
	}
	if closeErr != nil {
		_ = root.Remove(name)
		return fmt.Errorf("close materialized blob %q: %w", name, closeErr)
	}
	return nil
}

func makeMaterializedDirectory(root *os.Root, name string) error {
	if err := validateTreePath(name); err != nil {
		return err
	}
	if err := root.MkdirAll(name, 0o700); err != nil {
		return fmt.Errorf("materialize gitlink directory %q: %w", name, err)
	}
	return nil
}

func validateTreePath(name string) error {
	first, _, _ := strings.Cut(name, "/")
	if !fs.ValidPath(name) ||
		name == "." ||
		strings.EqualFold(first, ".git") ||
		strings.EqualFold(first, ".agentdock") {
		return fmt.Errorf("unsafe Git tree path %q", name)
	}
	return nil
}

func cleanupKnownWorktree(repository, root, workspace string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return cleanupKnownWorktreeContext(ctx, repository, root, workspace)
}

func cleanupKnownWorktreeContext(ctx context.Context, repository, root, workspace string) error {
	if !pathWithin(root, workspace) || workspace == root {
		return errors.New("refusing to clean worktree outside configured root")
	}
	command := safeGitCommand(ctx, repository, "worktree", "remove", "--force", workspace)
	output, removeErr := command.CombinedOutput()
	registered, inspectErr := worktreeRegistered(ctx, repository, workspace)
	if inspectErr != nil {
		if removeErr == nil {
			return fmt.Errorf("verify removed Git worktree registration: %w", inspectErr)
		}
		return errors.Join(
			fmt.Errorf("remove Git worktree: %w: %s", removeErr, strings.TrimSpace(string(output))),
			inspectErr,
		)
	}
	if registered {
		if removeErr == nil {
			return errors.New("worktree remains registered after successful remove")
		}
		return fmt.Errorf("remove Git worktree: %w: %s", removeErr, strings.TrimSpace(string(output)))
	}
	if err := os.RemoveAll(workspace); err != nil {
		return fmt.Errorf("remove unregistered disposable worktree: %w", err)
	}
	return nil
}

func worktreeRegistered(ctx context.Context, repository, workspace string) (bool, error) {
	list, err := gitOutput(ctx, repository, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	cleanWorkspace := filepath.Clean(workspace)
	for _, line := range strings.Split(list, "\n") {
		if value, ok := strings.CutPrefix(line, "worktree "); ok &&
			filepath.Clean(value) == cleanWorkspace {
			return true, nil
		}
	}
	return false, nil
}

func isObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}

func safeName(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' ||
			character == '_' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('-')
		}
		if builder.Len() >= 40 {
			break
		}
	}
	if builder.Len() == 0 {
		return "scope"
	}
	return builder.String()
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err == nil &&
		!filepath.IsAbs(relative) &&
		(relative == "." ||
			(relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))) {
		return true
	}
	// filepath.Rel is lexical and case-sensitive even when the filesystem is
	// not. Compare existing ancestors by filesystem identity so a case or
	// Unicode alias cannot place the worktree root physically inside origin.
	rootInfo, statErr := os.Stat(root)
	if statErr != nil {
		return false
	}
	current := filepath.Clean(candidate)
	for {
		if info, currentErr := os.Stat(current); currentErr == nil && os.SameFile(rootInfo, info) {
			return true
		}
		next := filepath.Dir(current)
		if next == current {
			return false
		}
		current = next
	}
}
