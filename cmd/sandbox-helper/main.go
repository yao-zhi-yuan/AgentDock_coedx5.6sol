package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const workspace = "/workspace"

func main() {
	if len(os.Args) < 2 {
		fail("subcommand is required")
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		fail("open workspace root: %v", err)
	}
	defer root.Close()

	switch os.Args[1] {
	case "list":
		runList(root, argumentOrDefault(2, "."))
	case "read":
		if len(os.Args) < 3 || len(os.Args) > 5 {
			fail("read requires path and optional start/end line")
		}
		runRead(root, os.Args[2], optionalPositiveInt(3, 1), optionalPositiveInt(4, 0))
	case "search":
		if len(os.Args) != 4 {
			fail("search requires pattern and relative path")
		}
		runSearch(root, os.Args[2], os.Args[3])
	case "apply-patch":
		runApplyPatch(root)
	case "test":
		runTest(root)
	case "probe":
		if len(os.Args) != 3 {
			fail("probe name is required")
		}
		runProbe(root, os.Args[2])
	case "cleanup":
		if len(os.Args) != 2 {
			fail("cleanup does not accept arguments")
		}
		runCleanup(root)
	default:
		fail("unknown subcommand %q", os.Args[1])
	}
}

func runList(root *os.Root, candidate string) {
	clean := safeRelative(candidate)
	var paths []string
	err := fs.WalkDir(root.FS(), clean, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if reservedPath(name) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() {
			paths = append(paths, name)
		}
		return nil
	})
	if err != nil {
		fail("list %q: %v", clean, err)
	}
	sort.Strings(paths)
	for _, name := range paths {
		fmt.Println(name)
	}
}

func runRead(root *os.Root, candidate string, start, end int) {
	clean := safeRelative(candidate)
	file, err := root.Open(clean)
	if err != nil {
		fail("read %q: %v", clean, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		if line < start {
			continue
		}
		if end > 0 && line > end {
			break
		}
		fmt.Println(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		fail("read %q: %v", clean, err)
	}
}

func runSearch(root *os.Root, pattern, candidate string) {
	clean := safeRelative(candidate)
	err := fs.WalkDir(root.FS(), clean, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if reservedPath(name) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			if strings.Contains(scanner.Text(), pattern) {
				fmt.Printf("%s:%d:%s\n", name, line, scanner.Text())
			}
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return scanErr
		}
		return closeErr
	})
	if err != nil {
		fail("search %q: %v", clean, err)
	}
}

type patchRequest struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

type testRequest struct {
	Packages []string `json:"packages"`
	Run      string   `json:"run,omitempty"`
	Verbose  bool     `json:"verbose,omitempty"`
}

func runApplyPatch(root *os.Root) {
	var request patchRequest
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		fail("decode patch: %v", err)
	}
	clean := safeRelative(request.Path)
	if request.Old == "" {
		fail("patch old text must be non-empty")
	}
	if strings.Contains(request.New, request.Old) {
		fail("patch new text must not retain old text; replay must be rejected")
	}
	content, err := root.ReadFile(clean)
	if err != nil {
		fail("read patch target %q: %v", clean, err)
	}
	if count := strings.Count(string(content), request.Old); count != 1 {
		fail("patch old text occurs %d times, want exactly one", count)
	}
	updated := strings.Replace(string(content), request.Old, request.New, 1)
	if strings.Contains(updated, request.Old) {
		fail("patch result still contains old text; replay must be rejected")
	}
	info, err := root.Stat(clean)
	if err != nil {
		fail("stat patch target %q: %v", clean, err)
	}
	temporary := path.Join(path.Dir(clean), ".agentdock-patch-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		fail("create patch temporary: %v", err)
	}
	if _, err := io.WriteString(file, updated); err != nil {
		file.Close()
		_ = root.Remove(temporary)
		fail("write patch temporary: %v", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = root.Remove(temporary)
		fail("sync patch temporary: %v", err)
	}
	if err := file.Close(); err != nil {
		_ = root.Remove(temporary)
		fail("close patch temporary: %v", err)
	}
	if err := root.Rename(temporary, clean); err != nil {
		_ = root.Remove(temporary)
		fail("publish patch: %v", err)
	}
	fmt.Printf("patched %s\n", clean)
}

func runTest(root *os.Root) {
	var request testRequest
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		fail("decode test request: %v", err)
	}
	if len(request.Packages) == 0 {
		fail("at least one test package is required")
	}
	for _, directory := range []string{
		".agentdock",
		".agentdock/home",
		".agentdock/tmp",
		".agentdock/go-cache",
	} {
		if err := root.MkdirAll(directory, 0o777); err != nil {
			fail("create test workspace state: %v", err)
		}
		if err := root.Chmod(directory, 0o777); err != nil {
			fail("set test workspace state permissions: %v", err)
		}
	}
	_ = syscall.Umask(0)
	arguments := []string{"test", "-count=1"}
	if request.Verbose {
		arguments = append(arguments, "-v")
	}
	if request.Run != "" {
		if strings.HasPrefix(request.Run, "-") || strings.ContainsRune(request.Run, 0) {
			fail("unsafe test run expression")
		}
		arguments = append(arguments, "-run", request.Run)
	}
	for _, name := range request.Packages {
		if name != "." && !strings.HasPrefix(name, "./") {
			fail("test package %q is outside the workspace", name)
		}
		clean := strings.TrimSuffix(name, "/...")
		if clean == "" {
			clean = "."
		}
		_ = safeRelative(clean)
		arguments = append(arguments, name)
	}
	command := exec.Command("go", arguments...)
	command.Dir = workspace
	command.Stdin = nil
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		fail("run go test: %v", err)
	}
}

func runCleanup(root *os.Root) {
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		first, _, _ := strings.Cut(name, "/")
		if strings.EqualFold(first, ".git") {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) {
			return nil
		}
		if entry.IsDir() {
			return root.Chmod(name, 0o777)
		}
		if entry.Type().IsRegular() {
			return root.Chmod(name, 0o666)
		}
		return nil
	})
	if err != nil {
		fail("cleanup workspace permissions: %v", err)
	}
}

func runProbe(root *os.Root, name string) {
	switch name {
	case "user":
		fmt.Printf("uid=%d gid=%d\n", os.Getuid(), os.Getgid())
	case "rootfs-write":
		if err := os.WriteFile("/agentdock-rootfs-probe", []byte("deny"), 0o600); err != nil {
			fail("rootfs-write-denied: %v", err)
		}
		fmt.Println("rootfs-write-unexpectedly-succeeded")
	case "nonworkspace-write":
		var writable []string
		for _, candidate := range []string{
			"/tmp/agentdock-write-probe",
			"/run/agentdock-write-probe",
			"/dev/shm/agentdock-write-probe",
		} {
			if err := os.WriteFile(candidate, []byte("deny"), 0o600); err == nil {
				writable = append(writable, candidate)
				_ = os.Remove(candidate)
			}
		}
		if len(writable) > 0 {
			fail("non-workspace paths were writable: %v", writable)
		}
		fmt.Println("non-workspace-write-denied")
	case "workspace-write":
		if err := root.MkdirAll(".agentdock", 0o777); err != nil {
			fail("create workspace state: %v", err)
		}
		if err := root.Chmod(".agentdock", 0o777); err != nil {
			fail("set workspace state permissions: %v", err)
		}
		if err := root.WriteFile(".agentdock/write-probe", []byte("ok"), 0o600); err != nil {
			fail("write workspace: %v", err)
		}
		fmt.Println("workspace-write-ok")
	case "workspace-directory-mode":
		info, err := os.Stat(workspace)
		if err != nil {
			fail("stat workspace directory: %v", err)
		}
		fmt.Printf(
			"workspace-sticky=%t workspace-other-write=%t\n",
			info.Mode()&os.ModeSticky != 0,
			info.Mode().Perm()&0o002 != 0,
		)
	case "git-pointer-delete":
		if err := root.Remove(".git"); err == nil {
			fail("sanitized Git pointer was removable")
		}
		if _, err := root.Stat(".git"); err != nil {
			fail("sanitized Git pointer disappeared: %v", err)
		}
		fmt.Println("git-pointer-delete-denied")
	case "restrict-workspace":
		if err := root.MkdirAll("locked/by-test", 0o700); err != nil {
			fail("create restricted workspace directory: %v", err)
		}
		if err := root.WriteFile("locked/by-test/file", []byte("cleanup"), 0o600); err != nil {
			fail("write restricted workspace file: %v", err)
		}
		if err := root.Chmod("locked/by-test", 0o700); err != nil {
			fail("restrict workspace directory: %v", err)
		}
		if err := root.Chmod("locked", 0o700); err != nil {
			fail("restrict workspace parent: %v", err)
		}
		fmt.Println("workspace-restricted")
	case "network":
		connection, err := net.DialTimeout("tcp", "1.1.1.1:80", time.Second)
		if err != nil {
			fail("network-denied: %v", err)
		}
		connection.Close()
		fmt.Println("network-unexpectedly-succeeded")
	case "sleep":
		time.Sleep(10 * time.Second)
	case "pids":
		runPIDsProbe()
	case "large-output":
		_, _ = io.CopyN(os.Stdout, strings.NewReader(strings.Repeat("x", 1<<20)), 1<<20)
	case "limits":
		for _, file := range []string{
			"/sys/fs/cgroup/cpu.max",
			"/sys/fs/cgroup/memory.max",
			"/sys/fs/cgroup/pids.max",
		} {
			content, err := os.ReadFile(file)
			if err != nil {
				fail("read cgroup limit %s: %v", file, err)
			}
			fmt.Printf("%s=%s\n", path.Base(file), strings.TrimSpace(string(content)))
		}
	case "environment-keys":
		var keys []string
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Println(key)
		}
	case "fixed-environment":
		expected := map[string]string{
			"GOCACHE":     "/workspace/.agentdock/go-cache",
			"GOENV":       "off",
			"GOFLAGS":     "",
			"GOTOOLCHAIN": "local",
			"GOTMPDIR":    "/workspace/.agentdock/tmp",
			"HOME":        "/workspace/.agentdock/home",
			"TMPDIR":      "/workspace/.agentdock/tmp",
		}
		for key, value := range expected {
			if os.Getenv(key) != value {
				fail("fixed environment mismatch for %s", key)
			}
		}
		fmt.Println("fixed-environment-ok")
	case "toctou":
		runTOCTOUProbe(root)
	default:
		fail("unknown probe %q", name)
	}
}

func runTOCTOUProbe(root *os.Root) {
	const (
		safeName = ".agentdock-toctou-safe"
		linkName = ".agentdock-toctou-link"
	)
	if err := root.WriteFile(safeName, []byte("safe"), 0o600); err != nil {
		fail("create TOCTOU safe file: %v", err)
	}
	defer root.Remove(safeName)
	defer root.Remove(linkName)
	stop := make(chan struct{})
	var toggler sync.WaitGroup
	toggler.Add(1)
	go func() {
		defer toggler.Done()
		link := path.Join(workspace, linkName)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(link)
			_ = os.Symlink(safeName, link)
			_ = os.Remove(link)
			_ = os.Symlink("/etc/passwd", link)
		}
	}()
	for index := 0; index < 2000; index++ {
		content, err := root.ReadFile(linkName)
		if err != nil {
			continue
		}
		if string(content) != "safe" {
			close(stop)
			toggler.Wait()
			fail("TOCTOU escape observed unexpected bytes")
		}
	}
	close(stop)
	toggler.Wait()
	fmt.Println("toctou-escape-observed=false")
}

func runPIDsProbe() {
	const requested = 256
	commands := make([]*exec.Cmd, 0, requested)
	failures := 0
	for index := 0; index < requested; index++ {
		command := exec.Command("sleep", "5")
		if err := command.Start(); err != nil {
			failures++
			continue
		}
		commands = append(commands, command)
	}
	for _, command := range commands {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
	}
	fmt.Printf(
		"pids-limited=%t requested=%d started=%d rejected=%d\n",
		failures > 0,
		requested,
		len(commands),
		failures,
	)
	if failures == 0 {
		fail("PID limit was not observed")
	}
}

func safeRelative(candidate string) string {
	if candidate == "" || path.IsAbs(candidate) || strings.ContainsRune(candidate, 0) {
		fail("unsafe path %q", candidate)
	}
	clean := path.Clean(candidate)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		fail("unsafe path %q", candidate)
	}
	if !fs.ValidPath(clean) {
		fail("unsafe path %q", candidate)
	}
	if reservedPath(clean) {
		fail("reserved runtime path %q", candidate)
	}
	return clean
}

func reservedPath(candidate string) bool {
	first, _, _ := strings.Cut(candidate, "/")
	return strings.EqualFold(first, ".git") ||
		strings.EqualFold(first, ".agentdock")
}

func argumentOrDefault(index int, fallback string) string {
	if len(os.Args) > index {
		return os.Args[index]
	}
	return fallback
}

func optionalPositiveInt(index, fallback int) int {
	if len(os.Args) <= index {
		return fallback
	}
	value, err := strconv.Atoi(os.Args[index])
	if err != nil || value < 0 {
		fail("line number %q is invalid", os.Args[index])
	}
	return value
}

func fail(format string, values ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", values...)
	os.Exit(1)
}
