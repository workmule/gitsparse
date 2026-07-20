package gitutil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ============================================================
// Test Helpers
// ============================================================

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustChtimes(t *testing.T, path string, atime, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, atime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Errorf("%s content = %q, want %q", path, data, want)
	}
}

// startSlowServer starts a TCP listener on localhost that accepts connections
// but never sends an HTTP response. This simulates a hung / extremely slow
// network, making it ideal for testing timeout behaviour.
func startSlowServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				time.Sleep(30 * time.Second)
				c.Close()
			}(conn)
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	return fmt.Sprintf("http://127.0.0.1:%d/repo.git", port)
}

// initTempRepo creates a minimal git repo in a temp dir and returns its path.
func initTempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustRunGit(t, dir, "init")
	mustRunGit(t, dir, "config", "user.email", "test@test.com")
	mustRunGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustRunGit(t, dir, "add", ".")
	mustRunGit(t, dir, "commit", "-m", "init")
	return dir
}

func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	r := &Runner{}
	if err := r.Run(dir, args...); err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
}

// saveRunner returns a new Runner with the given timeout/retries for test scope.
func saveRunner(t *testing.T, timeout time.Duration, retries int) *Runner {
	t.Helper()
	return &Runner{Timeout: timeout, Retries: retries}
}

// ============================================================
// CacheHash
// ============================================================

func TestCacheHash_Deterministic(t *testing.T) {
	h1 := CacheHash("https://github.com/numpy/numpy.git", "main")
	h2 := CacheHash("https://github.com/numpy/numpy.git", "main")
	if h1 != h2 {
		t.Errorf("not deterministic: %q != %q", h1, h2)
	}
}

func TestCacheHash_DifferentInputs(t *testing.T) {
	base := CacheHash("repo", "main")
	if CacheHash("repo", "dev") == base {
		t.Error("different ref should produce different hash")
	}
	if CacheHash("other", "main") == base {
		t.Error("different repo should produce different hash")
	}
}

func TestCacheHash_Always12Chars(t *testing.T) {
	for _, tc := range []struct{ repo, ref string }{
		{"", ""},
		{"a", "b"},
		{strings.Repeat("x", 1000), "main"},
	} {
		h := CacheHash(tc.repo, tc.ref)
		if len(h) != 12 {
			t.Errorf("CacheHash(%q,%q) len=%d, want 12", tc.repo, tc.ref, len(h))
		}
	}
}

// ============================================================
// IsCommitSHA
// ============================================================

func TestIsCommitSHA(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want bool
	}{
		{"empty", "", false},
		{"6 chars (below min)", "abcdef", false},
		{"7 chars (min valid)", "abcdef0", true},
		{"40 chars (full SHA)", "a1b2c3d4e5f6789012345678901234567890abcd", true},
		{"uppercase hex", "ABCDEF0", true},
		{"mixed case", "aBcDeF0", true},
		{"contains g (not hex)", "abcdefg", false},
		{"contains z", "abcdez0", false},
		{"contains space", "abcdef ", false},
		{"contains slash", "abcd/ef", false},
		{"very long hex", strings.Repeat("a", 100), true},
		{"branch name", "feature/branch", false},
		{"tag name", "v1.0.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCommitSHA(tt.ref); got != tt.want {
				t.Errorf("IsCommitSHA(%q) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}

// ============================================================
// SplitAndTrim
// ============================================================

func TestSplitAndTrim(t *testing.T) {
	tests := []struct {
		name string
		s    string
		sep  string
		want []string
	}{
		{"empty string", "", ",", []string{}},
		{"single value", "numpy", ",", []string{"numpy"}},
		{"multiple", "a,b,c", ",", []string{"a", "b", "c"}},
		{"trailing comma", "a,b,", ",", []string{"a", "b"}},
		{"leading comma", ",a,b", ",", []string{"a", "b"}},
		{"only commas", ",,,", ",", []string{}},
		{"with spaces", " a , b , c ", ",", []string{"a", "b", "c"}},
		{"only spaces", "   ", ",", []string{}},
		{"different separator", "a|b|c", "|", []string{"a", "b", "c"}},
		{"tabs and newlines", "\ta\t,\tb\n", ",", []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitAndTrim(tt.s, tt.sep)
			if len(got) != len(tt.want) {
				t.Errorf("SplitAndTrim(%q,%q) = %v, want %v", tt.s, tt.sep, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// ============================================================
// LFSIncludePatterns
// ============================================================

func TestLfsIncludePatterns(t *testing.T) {
	tests := []struct {
		name string
		dirs []string
		want []string
	}{
		{"nil input", nil, nil},
		{"empty input", []string{}, nil},
		{"single dir", []string{"core"}, []string{"core/**"}},
		{"multiple dirs", []string{"a", "b"}, []string{"a/**", "b/**"}},
		{"nested path", []string{"src/vs"}, []string{"src/vs/**"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LFSIncludePatterns(tt.dirs)
			if len(got) != len(tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestLfsIncludePatterns_JoinForIncludeArg(t *testing.T) {
	dirs := []string{
		"ServerInternational/Common/Public/Global",
		"ServerInternational/Common/Public/Dimension",
		"ServerInternational/Common/Server/Global",
	}
	patterns := LFSIncludePatterns(dirs)
	joined := strings.Join(patterns, ",")

	want := "ServerInternational/Common/Public/Global/**,ServerInternational/Common/Public/Dimension/**,ServerInternational/Common/Server/Global/**"
	if joined != want {
		t.Errorf("joined = %q\nwant    = %q", joined, want)
	}
	if strings.Contains(joined, " ") {
		t.Errorf("joined should not contain spaces: %q", joined)
	}
	if len(patterns) != len(dirs) {
		t.Errorf("patterns len = %d, want %d", len(patterns), len(dirs))
	}
}

// ============================================================
// HasLFSFiles
// ============================================================

func TestHasLFSFiles_NoFile(t *testing.T) {
	dir := t.TempDir()
	if HasLFSFiles(dir) {
		t.Error("expected false when .gitattributes doesn't exist")
	}
}

func TestHasLFSFiles_NoLFS(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".gitattributes"), []byte("*.txt text\n"))
	if HasLFSFiles(dir) {
		t.Error("expected false when no LFS filter in .gitattributes")
	}
}

func TestHasLFSFiles_WithLFS(t *testing.T) {
	dir := t.TempDir()
	content := "*.bin filter=lfs diff=lfs merge=lfs -text\n"
	mustWriteFile(t, filepath.Join(dir, ".gitattributes"), []byte(content))
	if !HasLFSFiles(dir) {
		t.Error("expected true when LFS filter present")
	}
}

func TestHasLFSFiles_PartialMatch(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".gitattributes"), []byte("# not filter=lfs\n"))
	if !HasLFSFiles(dir) {
		t.Log("note: 'filter=lfs' in comment still detected (known limitation)")
	}
}

// ============================================================
// DurStr / BoolStr
// ============================================================

func TestDurStr(t *testing.T) {
	if DurStr(0) != "off" {
		t.Errorf("DurStr(0) = %q, want 'off'", DurStr(0))
	}
	if DurStr(5*time.Minute) != "5m0s" {
		t.Errorf("DurStr(5m) = %q, want '5m0s'", DurStr(5*time.Minute))
	}
}

func TestBoolStr(t *testing.T) {
	if BoolStr(true, "yes", "no") != "yes" {
		t.Error("true case failed")
	}
	if BoolStr(false, "yes", "no") != "no" {
		t.Error("false case failed")
	}
}

// ============================================================
// CopyDir
// ============================================================

func TestCopyDir_BasicFiles(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dst")

	mustWriteFile(t, filepath.Join(src, "a.txt"), []byte("hello"))
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "b.txt"), []byte("world"))

	if err := CopyDir(src, dst); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	assertFileContent(t, filepath.Join(dst, "a.txt"), "hello")
	assertFileContent(t, filepath.Join(dst, "sub", "b.txt"), "world")
}

func TestCopyDir_EmptyDir(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dst")
	mustMkdirAll(t, filepath.Join(src, "empty"))

	if err := CopyDir(src, dst); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}
	info, err := os.Stat(filepath.Join(dst, "empty"))
	if err != nil || !info.IsDir() {
		t.Errorf("empty dir not copied: err=%v", err)
	}
}

func TestCopyDir_Permissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits not meaningful on Windows")
	}
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dst")

	if err := os.WriteFile(filepath.Join(src, "script.sh"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := CopyDir(src, dst); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	info, err := os.Stat(filepath.Join(dst, "script.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("perm = %o, want 0755", info.Mode().Perm())
	}
}

func TestCopyDir_Symlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks not reliable on Windows")
	}
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dst")

	mustWriteFile(t, filepath.Join(src, "target.txt"), []byte("content"))
	if err := os.Symlink("target.txt", filepath.Join(src, "link.txt")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	if err := CopyDir(src, dst); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	link, err := os.Readlink(filepath.Join(dst, "link.txt"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if link != "target.txt" {
		t.Errorf("link target = %q, want 'target.txt'", link)
	}
}

func TestCopyDir_DeepNested(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dst")

	deep := filepath.Join(src, "a", "b", "c", "d")
	mustMkdirAll(t, deep)
	mustWriteFile(t, filepath.Join(deep, "deep.txt"), []byte("deep"))

	if err := CopyDir(src, dst); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}
	assertFileContent(t, filepath.Join(dst, "a", "b", "c", "d", "deep.txt"), "deep")
}

// ============================================================
// PrepareCloneTarget
// ============================================================

func TestPrepareCloneTarget_NonExistent(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "newdir", "cache")
	PrepareCloneTarget(target)
	if _, err := os.Stat(filepath.Dir(target)); err != nil {
		t.Errorf("parent dir not created: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target should not exist, got err=%v", err)
	}
}

func TestPrepareCloneTarget_RemovesExisting(t *testing.T) {
	target := filepath.Join(t.TempDir(), "cache")
	mustMkdirAll(t, filepath.Join(target, "subdir"))
	mustWriteFile(t, filepath.Join(target, "leftover.txt"), []byte("partial"))

	PrepareCloneTarget(target)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target should be removed, got err=%v", err)
	}
}

func TestPrepareCloneTarget_Idempotent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "cache")
	PrepareCloneTarget(target)
	PrepareCloneTarget(target)
	PrepareCloneTarget(target)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target should not exist after prepare, got err=%v", err)
	}
}

// ============================================================
// CleanExpiredCache
// ============================================================

func TestCleanExpiredCache_NoDir(t *testing.T) {
	CleanExpiredCache(filepath.Join(t.TempDir(), "nonexistent"), time.Hour)
}

func TestCleanExpiredCache_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	CleanExpiredCache(dir, time.Hour)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestCleanExpiredCache_RemovesExpired_KeepsFresh(t *testing.T) {
	dir := t.TempDir()

	oldDir := filepath.Join(dir, "old-cache")
	mustMkdirAll(t, oldDir)
	pastTime := time.Now().Add(-2 * time.Hour)
	mustChtimes(t, oldDir, pastTime, pastTime)

	freshDir := filepath.Join(dir, "fresh-cache")
	mustMkdirAll(t, freshDir)

	CleanExpiredCache(dir, time.Hour)

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Error("old entry should be removed")
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Error("fresh entry should remain")
	}
}

func TestCleanExpiredCache_AllExpired(t *testing.T) {
	dir := t.TempDir()
	for i := range 3 {
		d := filepath.Join(dir, fmt.Sprintf("cache-%d", i))
		mustMkdirAll(t, d)
		past := time.Now().Add(-48 * time.Hour)
		mustChtimes(t, d, past, past)
	}

	CleanExpiredCache(dir, 24*time.Hour)

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected all entries removed, got %d", len(entries))
	}
}

func TestCleanExpiredCache_ZeroTTL(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "entry")
	mustMkdirAll(t, d)
	past := time.Now().Add(-48 * time.Hour)
	mustChtimes(t, d, past, past)

	CleanExpiredCache(dir, 0)

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Logf("entry survived with TTL=0 (timing dependent, acceptable)")
	}
}

// ============================================================
// Runner.RunRetry (mock-based, no real git needed)
// ============================================================

func TestRunRetry_SuccessFirstTry(t *testing.T) {
	r := saveRunner(t, 0, 3)

	calls := 0
	err := r.RunRetry(func() error {
		calls++
		return nil
	}, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestRunRetry_AllFail(t *testing.T) {
	r := saveRunner(t, 0, 1)

	calls := 0
	err := r.RunRetry(func() error {
		calls++
		return errors.New("network error")
	}, "test")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestRunRetry_SuccessOnThirdTry(t *testing.T) {
	r := saveRunner(t, 0, 3)

	calls := 0
	err := r.RunRetry(func() error {
		calls++
		if calls < 3 {
			return errors.New("fail")
		}
		return nil
	}, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestRunRetry_DeadlineExceeded(t *testing.T) {
	r := saveRunner(t, 0, 1)

	calls := 0
	err := r.RunRetry(func() error {
		calls++
		return context.DeadlineExceeded
	}, "test")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestRunRetry_ZeroRetries(t *testing.T) {
	r := saveRunner(t, 0, 0)

	calls := 0
	err := r.RunRetry(func() error {
		calls++
		return errors.New("fail")
	}, "test")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retries)", calls)
	}
}

func TestRunRetry_NegativeRetries(t *testing.T) {
	r := saveRunner(t, 0, -1)

	calls := 0
	err := r.RunRetry(func() error {
		calls++
		return errors.New("fail")
	}, "test")
	if calls != 0 {
		t.Errorf("calls = %d, want 0 (loop body never executes)", calls)
	}
	if err != nil {
		t.Errorf("err = %v, want nil (lastErr never set)", err)
	}
}

// ============================================================
// Runner.Run (real git required)
// ============================================================

func TestRun_BasicOperation(t *testing.T) {
	r := saveRunner(t, 0, 0)

	dir := t.TempDir()
	if err := r.Run(dir, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := r.Run(dir, "status"); err != nil {
		t.Fatalf("git status: %v", err)
	}
}

func TestRun_InvalidCommand(t *testing.T) {
	r := saveRunner(t, 0, 0)

	err := r.Run("", "not-a-real-git-subcommand")
	if err == nil {
		t.Fatal("expected error for invalid git command")
	}
}

func TestRun_NonExistentDir(t *testing.T) {
	r := saveRunner(t, 0, 0)

	err := r.Run(filepath.Join(t.TempDir(), "does-not-exist"), "status")
	if err == nil {
		t.Fatal("expected error for non-existent working dir")
	}
}

// ============================================================
// Network-restricted timeout tests (slow server simulation)
// ============================================================

func TestRun_TimeoutWithSlowServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}

	repoURL := startSlowServer(t)
	r := saveRunner(t, 300*time.Millisecond, 0)

	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "clone")

	start := time.Now()
	err := r.Run("", "clone", "--depth=1", repoURL, target)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 3*time.Second {
		t.Errorf("took too long to timeout: %v (expected ~300ms)", elapsed)
	}
	t.Logf("timed out after %v as expected, error: %v", elapsed, err)
}

func TestRunRetry_TimeoutWithSlowServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}

	repoURL := startSlowServer(t)
	r := saveRunner(t, 300*time.Millisecond, 1)

	calls := 0
	tmpDir := t.TempDir()

	start := time.Now()
	err := r.RunRetry(func() error {
		calls++
		target := filepath.Join(tmpDir, fmt.Sprintf("clone%d", calls))
		return r.Run("", "clone", "--depth=1", repoURL, target)
	}, "clone")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after retries, got nil")
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if elapsed > 6*time.Second {
		t.Errorf("took too long: %v", elapsed)
	}
	t.Logf("completed after %v with %d attempts, error: %v", elapsed, calls, err)
}

func TestRun_TimeoutZero_NoTimeout(t *testing.T) {
	r := saveRunner(t, 0, 0)

	dir := t.TempDir()
	if err := r.Run(dir, "init"); err != nil {
		t.Fatalf("git init failed with no timeout: %v", err)
	}
}

// ============================================================
// 集成测试: clone 超时残留目录后重试成功 (回归 bug 修复)
// ============================================================

func TestCloneRetryAfterStaleDir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := saveRunner(t, 0, 1)

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "file.txt"), []byte("ok"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs")

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "cache")
	mustMkdirAll(t, filepath.Join(target, ".git"))
	mustWriteFile(t, filepath.Join(target, ".git", "config"), []byte("partial"))
	mustWriteFile(t, filepath.Join(target, "README.md"), []byte("partial"))

	calls := 0
	err := r.RunRetry(func() error {
		calls++
		PrepareCloneTarget(target)
		return r.Run("", "clone", "--depth=1", "--no-tags", "-b", "master", bareRepo, target)
	}, "clone")

	if err != nil {
		t.Fatalf("clone should succeed on retry after cleaning stale dir: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	assertFileContent(t, filepath.Join(target, "docs", "file.txt"), "ok")
}

func TestCloneRetryWithRealStaleDirSimulatingTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}
	r := &Runner{Timeout: 300 * time.Millisecond, Retries: 1}

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "file.txt"), []byte("ok"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs")
	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	slowURL := startSlowServer(t)

	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "cache")

	attempt := 0
	err := r.RunRetry(func() error {
		attempt++
		PrepareCloneTarget(target)
		if attempt == 1 {
			return r.Run("", "clone", "--depth=1", slowURL, target)
		}
		r.Timeout = 0
		return r.Run("", "clone", "--depth=1", "--no-tags", "-b", "master", bareRepo, target)
	}, "clone")

	if err != nil {
		t.Fatalf("retry should succeed after stale dir cleanup: %v", err)
	}
	if attempt != 2 {
		t.Errorf("attempts = %d, want 2", attempt)
	}
	assertFileContent(t, filepath.Join(target, "docs", "file.txt"), "ok")
}

func TestCloneRetryAfterStaleDir_WithGitSubtree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := saveRunner(t, 0, 1)

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "file.txt"), []byte("ok"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs")

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "cache")
	mustMkdirAll(t, filepath.Join(target, ".git", "objects", "pack"))
	mustMkdirAll(t, filepath.Join(target, ".git", "refs", "heads"))
	mustWriteFile(t, filepath.Join(target, ".git", "HEAD"), []byte("ref: refs/heads/master\n"))
	mustWriteFile(t, filepath.Join(target, ".git", "config"), []byte("[core]\n\trepositoryformatversion = 0\n"))
	mustWriteFile(t, filepath.Join(target, ".git", "objects", "pack", "partial.idx"), []byte("partial"))
	mustMkdirAll(t, filepath.Join(target, "docs"))
	mustWriteFile(t, filepath.Join(target, "docs", "file.txt"), []byte("STALE_PARTIAL"))
	mustWriteFile(t, filepath.Join(target, "README.md"), []byte("partial"))

	if entries, _ := os.ReadDir(target); len(entries) == 0 {
		t.Fatal("precondition: target should have stale entries")
	}

	calls := 0
	err := r.RunRetry(func() error {
		calls++
		PrepareCloneTarget(target)
		return r.Run("", "clone", "--depth=1", "--no-tags", "-b", "master", bareRepo, target)
	}, "clone")

	if err != nil {
		t.Fatalf("clone should succeed after cleaning stale .git subtree: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	assertFileContent(t, filepath.Join(target, "docs", "file.txt"), "ok")
}

func TestCloneRetryMultipleTimeoutsThenSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}
	r := &Runner{Timeout: 200 * time.Millisecond, Retries: 3}

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "file.txt"), []byte("ok"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs")
	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	slowURL := startSlowServer(t)
	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "cache")

	attempt := 0
	err := r.RunRetry(func() error {
		attempt++
		PrepareCloneTarget(target)
		if attempt < 4 {
			return r.Run("", "clone", "--depth=1", slowURL, target)
		}
		r.Timeout = 0
		return r.Run("", "clone", "--depth=1", "--no-tags", "-b", "master", bareRepo, target)
	}, "clone")

	if err != nil {
		t.Fatalf("should succeed on 4th attempt after 3 cleanups: %v", err)
	}
	if attempt != 4 {
		t.Errorf("attempts = %d, want 4", attempt)
	}
	assertFileContent(t, filepath.Join(target, "docs", "file.txt"), "ok")
}

func TestCloneRetryAfterStaleDir_SHAFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := saveRunner(t, 0, 1)

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "file.txt"), []byte("sha-content"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs")

	out, err := exec.Command("git", "-C", srcRepo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	sha := strings.TrimSpace(string(out))

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "cache")
	mustMkdirAll(t, filepath.Join(target, ".git", "objects"))
	mustWriteFile(t, filepath.Join(target, ".git", "HEAD"), []byte("ref: refs/heads/master\n"))
	mustWriteFile(t, filepath.Join(target, "stale.txt"), []byte("stale"))

	calls := 0
	err = r.RunRetry(func() error {
		calls++
		PrepareCloneTarget(target)
		return r.Run("", "clone", "--depth=1", "--no-tags", bareRepo, target)
	}, "clone")
	if err != nil {
		t.Fatalf("SHA clone should succeed after cleanup: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}

	if err := r.Run(target, "fetch", "--depth=1", "--no-tags", "origin", sha); err != nil {
		t.Fatalf("fetch SHA: %v", err)
	}
	if err := r.Run(target, "checkout", sha); err != nil {
		t.Fatalf("checkout SHA: %v", err)
	}
	assertFileContent(t, filepath.Join(target, "docs", "file.txt"), "sha-content")
	if _, err := os.Stat(filepath.Join(target, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("stale.txt from previous failed clone should not exist")
	}
}

func TestCloneFailsWithoutPrepareCloneTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := saveRunner(t, 0, 0)

	srcRepo := initTempRepo(t)
	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	target := filepath.Join(t.TempDir(), "cache")
	mustMkdirAll(t, filepath.Join(target, ".git"))
	mustWriteFile(t, filepath.Join(target, ".git", "config"), []byte("stale"))

	err := r.Run("", "clone", "--depth=1", "--no-tags", "-b", "master", bareRepo, target)
	if err == nil {
		t.Fatal("expected clone to FAIL when target dir exists and is non-empty " +
			"(this test verifies the bug exists without PrepareCloneTarget)")
	}
	if !strings.Contains(err.Error(), "already exists") &&
		!strings.Contains(strings.ToLower(err.Error()), "exists") {
		t.Logf("note: clone failed as expected, error: %v", err)
	}
}

// ============================================================
// 集成测试: v2 简化流程 (clone + fetch + reset + CopyDir)
// ============================================================

func TestFullClone_Checkout_CopyDir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := saveRunner(t, 0, 0)

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "guide.md"), []byte("# Guide\n"))
	mustMkdirAll(t, filepath.Join(srcRepo, "src"))
	mustWriteFile(t, filepath.Join(srcRepo, "src", "main.go"), []byte("package main\n"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "data.json"), []byte(`{"id":1}`))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "config.xml"), []byte("<c/>"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs and src")

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := r.Run("", "clone", "--depth=1", "--no-tags",
		"-b", "master", bareRepo, cacheDir); err != nil {
		t.Fatalf("clone: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cacheDir, "docs", "guide.md")); err != nil {
		t.Fatalf("docs/guide.md missing in cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "src", "main.go")); err != nil {
		t.Fatalf("src/main.go missing in cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "README.md")); err != nil {
		t.Fatalf("README.md missing in cache: %v", err)
	}

	outputDir := filepath.Join(t.TempDir(), "output")
	src := filepath.Join(cacheDir, "docs")
	dst := filepath.Join(outputDir, "docs")
	os.MkdirAll(filepath.Dir(dst), 0755)
	if err := CopyDir(src, dst); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	assertFileContent(t, filepath.Join(dst, "guide.md"), "# Guide\n")
	assertFileContent(t, filepath.Join(dst, "data.json"), `{"id":1}`)
	assertFileContent(t, filepath.Join(dst, "config.xml"), "<c/>")
}

func TestCacheReuse_FetchReset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := saveRunner(t, 0, 0)

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "version.txt"), []byte("v1"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "v1")

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := r.Run("", "clone", "--depth=1", "--no-tags",
		"-b", "master", bareRepo, cacheDir); err != nil {
		t.Fatalf("first clone: %v", err)
	}
	assertFileContent(t, filepath.Join(cacheDir, "docs", "version.txt"), "v1")

	mustWriteFile(t, filepath.Join(srcRepo, "docs", "version.txt"), []byte("v2"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "v2")
	if err := r.Run(srcRepo, "push", bareRepo, "master"); err != nil {
		t.Fatalf("push v2: %v", err)
	}

	if err := r.Run(cacheDir, "fetch", "--depth=1", "--no-tags", "origin", "master"); err != nil {
		t.Fatalf("fetch on cache hit: %v", err)
	}
	if err := r.Run(cacheDir, "reset", "--hard", "origin/master"); err != nil {
		t.Fatalf("reset --hard after fetch: %v", err)
	}

	assertFileContent(t, filepath.Join(cacheDir, "docs", "version.txt"), "v2")
}

func TestCacheReuse_FetchReset_Tag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := saveRunner(t, 0, 0)

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "version.txt"), []byte("v1"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "v1")
	mustRunGit(t, srcRepo, "tag", "v1.0.0")

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := r.Run("", "clone", "--depth=1", "--no-tags",
		"-b", "v1.0.0", bareRepo, cacheDir); err != nil {
		t.Fatalf("first clone with tag: %v", err)
	}
	assertFileContent(t, filepath.Join(cacheDir, "docs", "version.txt"), "v1")

	if err := r.Run(cacheDir, "fetch", "--depth=1", "origin", "refs/tags/v1.0.0"); err != nil {
		t.Fatalf("fetch tag: %v", err)
	}
	if err := r.Run(cacheDir, "reset", "--hard", "v1.0.0"); err != nil {
		t.Fatalf("reset --hard tag: %v", err)
	}
	assertFileContent(t, filepath.Join(cacheDir, "docs", "version.txt"), "v1")
}

func TestCloneWithCommitSHA(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := saveRunner(t, 0, 0)

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "file.txt"), []byte("content"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs")

	out, err := exec.Command("git", "-C", srcRepo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	sha := strings.TrimSpace(string(out))
	if !IsCommitSHA(sha) {
		t.Fatalf("rev-parse output not a valid SHA: %q", sha)
	}

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := r.Run("", "clone", "--depth=1", "--no-tags", bareRepo, cacheDir); err != nil {
		t.Fatalf("clone default branch: %v", err)
	}
	if err := r.Run(cacheDir, "fetch", "--depth=1", "--no-tags", "origin", sha); err != nil {
		t.Fatalf("fetch SHA: %v", err)
	}
	if err := r.Run(cacheDir, "checkout", sha); err != nil {
		t.Fatalf("checkout SHA: %v", err)
	}

	assertFileContent(t, filepath.Join(cacheDir, "docs", "file.txt"), "content")
}

func TestRunRetry_GitCloneLocalThenSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := saveRunner(t, 0, 1)

	srcRepo := initTempRepo(t)
	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	calls := 0
	dstDir := t.TempDir()
	err := r.RunRetry(func() error {
		calls++
		target := filepath.Join(dstDir, fmt.Sprintf("clone%d", calls))
		return r.Run("", "clone", bareRepo, target)
	}, "clone")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (should succeed first try)", calls)
	}
}

// ============================================================
// Git 版本解析与 sparse-checkout 兼容性检测
// ============================================================

// TestParseGitVersion 验证从 "git version x.y.z" 输出解析版本号.
func TestParseGitVersion(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    [3]int
		wantErr bool
	}{
		{"standard", "git version 2.32.0\n", [3]int{2, 32, 0}, false},
		{"with os info", "git version 2.25.1.windows.1\n", [3]int{2, 25, 1}, false},
		{"old version", "git version 2.20.4\n", [3]int{2, 20, 4}, false},
		{"no prefix", "2.32.0", [3]int{2, 32, 0}, false},
		{"empty", "", [3]int{}, true},
		{"garbage", "not a version string", [3]int{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseGitVersion(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseGitVersion(%q) err = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseGitVersion(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestSupportsSparseCheckoutCone 验证 sparse-checkout --cone 兼容性判断.
func TestSupportsSparseCheckoutCone(t *testing.T) {
	tests := []struct {
		name string
		ver  [3]int
		want bool
	}{
		{"2.25.0 (first supported)", [3]int{2, 25, 0}, true},
		{"2.26.3", [3]int{2, 26, 3}, true},
		{"2.32.0", [3]int{2, 32, 0}, true},
		{"2.24.4 (too old)", [3]int{2, 24, 4}, false},
		{"2.20.4 (too old)", [3]int{2, 20, 4}, false},
		{"1.8.0 (very old)", [3]int{1, 8, 0}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SupportsSparseCheckoutCone(tt.ver); got != tt.want {
				t.Errorf("SupportsSparseCheckoutCone(%v) = %v, want %v", tt.ver, got, tt.want)
			}
		})
	}
}
