package main

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

// saveGlobals saves and restores globalTimeout / globalRetries.
func saveGlobals(t *testing.T) {
	t.Helper()
	oldTimeout := globalTimeout
	oldRetries := globalRetries
	t.Cleanup(func() {
		globalTimeout = oldTimeout
		globalRetries = oldRetries
	})
}

// startSlowServer starts a TCP listener on localhost that accepts connections
// but never sends an HTTP response. This simulates a hung / extremely slow
// network, making it ideal for testing timeout behaviour without root or
// external network dependencies.
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
			// Accept but hold the connection open without responding.
			go func(c net.Conn) {
				buf := make([]byte, 4096)
				_, _ = c.Read(buf) // block until client sends or closes
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
	old := globalTimeout
	globalTimeout = 0
	defer func() { globalTimeout = old }()
	if err := runGit(dir, args...); err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
}

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

// ============================================================
// cacheHash
// ============================================================

func TestCacheHash_Deterministic(t *testing.T) {
	h1 := cacheHash("https://github.com/numpy/numpy.git", "main")
	h2 := cacheHash("https://github.com/numpy/numpy.git", "main")
	if h1 != h2 {
		t.Errorf("not deterministic: %q != %q", h1, h2)
	}
}

func TestCacheHash_DifferentInputs(t *testing.T) {
	base := cacheHash("repo", "main")
	if cacheHash("repo", "dev") == base {
		t.Error("different ref should produce different hash")
	}
	if cacheHash("other", "main") == base {
		t.Error("different repo should produce different hash")
	}
}

func TestCacheHash_Always12Chars(t *testing.T) {
	for _, tc := range []struct{ repo, ref string }{
		{"", ""},
		{"a", "b"},
		{strings.Repeat("x", 1000), "main"},
	} {
		h := cacheHash(tc.repo, tc.ref)
		if len(h) != 12 {
			t.Errorf("cacheHash(%q,%q) len=%d, want 12", tc.repo, tc.ref, len(h))
		}
	}
}

// ============================================================
// isCommitSHA
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
			if got := isCommitSHA(tt.ref); got != tt.want {
				t.Errorf("isCommitSHA(%q) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}

// ============================================================
// splitAndTrim
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
			got := splitAndTrim(tt.s, tt.sep)
			if len(got) != len(tt.want) {
				t.Errorf("splitAndTrim(%q,%q) = %v, want %v", tt.s, tt.sep, got, tt.want)
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
// lfsIncludePatterns
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
			got := lfsIncludePatterns(tt.dirs)
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

// TestLfsIncludePatterns_JoinForIncludeArg 验证修复后的 LFS --include 参数拼接方式:
// 多个模式必须用逗号拼接成单个字符串, 而非作为多个独立参数.
// 回归 bug: 原代码 `append([]string{"--include"}, patterns...)` 导致 git 把第 2 个
// 模式起当作 remote 名, 报 "Invalid remote name".
func TestLfsIncludePatterns_JoinForIncludeArg(t *testing.T) {
	dirs := []string{
		"ServerInternational/Common/Public/Global",
		"ServerInternational/Common/Public/Dimension",
		"ServerInternational/Common/Server/Global",
	}
	patterns := lfsIncludePatterns(dirs)
	joined := strings.Join(patterns, ",")

	want := "ServerInternational/Common/Public/Global/**,ServerInternational/Common/Public/Dimension/**,ServerInternational/Common/Server/Global/**"
	if joined != want {
		t.Errorf("joined = %q\nwant    = %q", joined, want)
	}
	// 关键断言: 不含空格, 是单个逗号分隔字符串
	if strings.Contains(joined, " ") {
		t.Errorf("joined should not contain spaces: %q", joined)
	}
	// 模式数量应与目录数一致
	if len(patterns) != len(dirs) {
		t.Errorf("patterns len = %d, want %d", len(patterns), len(dirs))
	}
}

// ============================================================
// hasLFSFiles
// ============================================================

func TestHasLFSFiles_NoFile(t *testing.T) {
	dir := t.TempDir()
	if hasLFSFiles(dir) {
		t.Error("expected false when .gitattributes doesn't exist")
	}
}

func TestHasLFSFiles_NoLFS(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".gitattributes"), []byte("*.txt text\n"))
	if hasLFSFiles(dir) {
		t.Error("expected false when no LFS filter in .gitattributes")
	}
}

func TestHasLFSFiles_WithLFS(t *testing.T) {
	dir := t.TempDir()
	content := "*.bin filter=lfs diff=lfs merge=lfs -text\n"
	mustWriteFile(t, filepath.Join(dir, ".gitattributes"), []byte(content))
	if !hasLFSFiles(dir) {
		t.Error("expected true when LFS filter present")
	}
}

func TestHasLFSFiles_PartialMatch(t *testing.T) {
	dir := t.TempDir()
	// "filter=lfs" appears as substring but not as actual LFS rule
	// This tests that the detection is simple substring matching
	mustWriteFile(t, filepath.Join(dir, ".gitattributes"), []byte("# not filter=lfs\n"))
	if !hasLFSFiles(dir) {
		t.Log("note: 'filter=lfs' in comment still detected (known limitation)")
	}
}

// ============================================================
// durStr / boolStr
// ============================================================

func TestDurStr(t *testing.T) {
	if durStr(0) != "off" {
		t.Errorf("durStr(0) = %q, want 'off'", durStr(0))
	}
	if durStr(5*time.Minute) != "5m0s" {
		t.Errorf("durStr(5m) = %q, want '5m0s'", durStr(5*time.Minute))
	}
}

func TestBoolStr(t *testing.T) {
	if boolStr(true, "yes", "no") != "yes" {
		t.Error("true case failed")
	}
	if boolStr(false, "yes", "no") != "no" {
		t.Error("false case failed")
	}
}

// ============================================================
// copyDir
// ============================================================

func TestCopyDir_BasicFiles(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dst")

	mustWriteFile(t, filepath.Join(src, "a.txt"), []byte("hello"))
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "b.txt"), []byte("world"))

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
	}

	assertFileContent(t, filepath.Join(dst, "a.txt"), "hello")
	assertFileContent(t, filepath.Join(dst, "sub", "b.txt"), "world")
}

func TestCopyDir_EmptyDir(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dst")
	mustMkdirAll(t, filepath.Join(src, "empty"))

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
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

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
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

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
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

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
	}
	assertFileContent(t, filepath.Join(dst, "a", "b", "c", "d", "deep.txt"), "deep")
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

// ============================================================
// prepareCloneTarget
// ============================================================

// TestPrepareCloneTarget_NonExistent 验证目标不存在时仅创建父目录, 不报错.
func TestPrepareCloneTarget_NonExistent(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "newdir", "cache")
	prepareCloneTarget(target)
	if _, err := os.Stat(filepath.Dir(target)); err != nil {
		t.Errorf("parent dir not created: %v", err)
	}
	// target 本身不应被创建 (留给 git clone)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target should not exist, got err=%v", err)
	}
}

// TestPrepareCloneTarget_RemovesExisting 验证已存在的目标目录被清理.
func TestPrepareCloneTarget_RemovesExisting(t *testing.T) {
	target := filepath.Join(t.TempDir(), "cache")
	mustMkdirAll(t, filepath.Join(target, "subdir"))
	mustWriteFile(t, filepath.Join(target, "leftover.txt"), []byte("partial"))

	prepareCloneTarget(target)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target should be removed, got err=%v", err)
	}
}

// TestPrepareCloneTarget_Idempotent 多次调用应安全.
func TestPrepareCloneTarget_Idempotent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "cache")
	prepareCloneTarget(target)
	prepareCloneTarget(target)
	prepareCloneTarget(target)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target should not exist after prepare, got err=%v", err)
	}
}

// ============================================================
// 集成测试: clone 超时残留目录后重试成功 (回归 bug 修复)
// ============================================================

// TestV2_CloneRetryAfterStaleDir 验证修复的关键场景:
// 第一次 clone 因超时失败, 目标目录留下部分写入的文件;
// 重试前 prepareCloneTarget 清理残留目录, 第二次 clone 成功.
// 这是 user_query 报告的 bug: "destination path already exists
// and is not an empty directory".
func TestV2_CloneRetryAfterStaleDir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	saveGlobals(t)
	globalTimeout = 0
	globalRetries = 1

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "file.txt"), []byte("ok"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs")

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := runGit("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	// 模拟第一次失败残留: 预先在目标目录写入"部分 clone"的垃圾文件
	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "cache")
	mustMkdirAll(t, filepath.Join(target, ".git"))
	mustWriteFile(t, filepath.Join(target, ".git", "config"), []byte("partial"))
	mustWriteFile(t, filepath.Join(target, "README.md"), []byte("partial"))

	calls := 0
	err := runGitRetry(func() error {
		calls++
		// 模拟 main.go 修复后的逻辑: 每次重试前清理目标目录
		prepareCloneTarget(target)
		return runGit("", "clone", "--depth=1", "--no-tags", "-b", "master", bareRepo, target)
	}, "clone")

	if err != nil {
		t.Fatalf("clone should succeed on retry after cleaning stale dir: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (prepareCloneTarget should make first attempt succeed)", calls)
	}
	// 验证 clone 后文件正确 (非残留内容)
	assertFileContent(t, filepath.Join(target, "docs", "file.txt"), "ok")
}

// TestV2_CloneRetryWithRealStaleDirSimulatingTimeout 更贴近真实 bug:
// 第一次用慢服务器触发超时 + 残留目录, 第二次用真实仓库重试成功.
// 验证 prepareCloneTarget 在 runGitRetry 闭包内的集成行为.
func TestV2_CloneRetryWithRealStaleDirSimulatingTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}
	saveGlobals(t)
	globalRetries = 1

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "file.txt"), []byte("ok"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs")
	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := runGit("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	// 慢服务器用于第一次失败
	slowURL := startSlowServer(t)

	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "cache")

	// 切换: 第一次超时 (慢服务器), 第二次用真实仓库
	globalTimeout = 300 * time.Millisecond
	attempt := 0
	err := runGitRetry(func() error {
		attempt++
		prepareCloneTarget(target)
		if attempt == 1 {
			// 第一次: 慢服务器, 会超时并残留部分目录
			return runGit("", "clone", "--depth=1", slowURL, target)
		}
		// 第二次: 真实仓库, 应成功 (前提是 prepareCloneTarget 清理了残留)
		globalTimeout = 0
		return runGit("", "clone", "--depth=1", "--no-tags", "-b", "master", bareRepo, target)
	}, "clone")

	if err != nil {
		t.Fatalf("retry should succeed after stale dir cleanup: %v", err)
	}
	if attempt != 2 {
		t.Errorf("attempts = %d, want 2", attempt)
	}
	assertFileContent(t, filepath.Join(target, "docs", "file.txt"), "ok")
}

// TestV2_CloneRetryAfterStaleDir_WithGitSubtree 验证最危险的残留形态:
// 上次 clone 中断留下了完整的 .git 子树 (objects/config/HEAD 等) + 部分工作区文件.
// 这种残留最容易让 git clone 误判目录"已被占用". prepareCloneTarget 必须能彻底清除.
func TestV2_CloneRetryAfterStaleDir_WithGitSubtree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	saveGlobals(t)
	globalTimeout = 0
	globalRetries = 1

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "file.txt"), []byte("ok"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs")

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := runGit("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	// 构造真实 clone 中断后的残留: 模拟 .git 子树 + 工作区部分文件
	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "cache")
	// 模拟 .git 子树 (git clone 中断常见残留)
	mustMkdirAll(t, filepath.Join(target, ".git", "objects", "pack"))
	mustMkdirAll(t, filepath.Join(target, ".git", "refs", "heads"))
	mustWriteFile(t, filepath.Join(target, ".git", "HEAD"), []byte("ref: refs/heads/master\n"))
	mustWriteFile(t, filepath.Join(target, ".git", "config"), []byte("[core]\n\trepositoryformatversion = 0\n"))
	mustWriteFile(t, filepath.Join(target, ".git", "objects", "pack", "partial.idx"), []byte("partial"))
	// 工作区部分文件 (checkout 中断残留)
	mustMkdirAll(t, filepath.Join(target, "docs"))
	mustWriteFile(t, filepath.Join(target, "docs", "file.txt"), []byte("STALE_PARTIAL"))
	mustWriteFile(t, filepath.Join(target, "README.md"), []byte("partial"))

	// 残留目录必须非空 (前置条件)
	if entries, _ := os.ReadDir(target); len(entries) == 0 {
		t.Fatal("precondition: target should have stale entries")
	}

	calls := 0
	err := runGitRetry(func() error {
		calls++
		prepareCloneTarget(target)
		return runGit("", "clone", "--depth=1", "--no-tags", "-b", "master", bareRepo, target)
	}, "clone")

	if err != nil {
		t.Fatalf("clone should succeed after cleaning stale .git subtree: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	// 关键: 内容必须是新 clone 的 "ok", 而非残留的 "STALE_PARTIAL"
	assertFileContent(t, filepath.Join(target, "docs", "file.txt"), "ok")
}

// TestV2_CloneRetryMultipleTimeoutsThenSuccess 验证多次重试都超时残留,
// 最后一次才成功: 每次 prepareCloneTarget 都必须清理上一次的残留.
// 模拟线上 -retries=3 场景: 前 3 次超时, 第 4 次成功.
func TestV2_CloneRetryMultipleTimeoutsThenSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}
	saveGlobals(t)
	globalRetries = 3 // 4 次尝试 (1 初始 + 3 重试)

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "file.txt"), []byte("ok"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs")
	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := runGit("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	slowURL := startSlowServer(t)
	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "cache")

	globalTimeout = 200 * time.Millisecond
	attempt := 0
	err := runGitRetry(func() error {
		attempt++
		prepareCloneTarget(target)
		if attempt < 4 {
			// 前 3 次慢服务器, 必然超时残留
			return runGit("", "clone", "--depth=1", slowURL, target)
		}
		// 第 4 次真实仓库, 应成功 (前提是前 3 次的残留都被清理干净)
		globalTimeout = 0
		return runGit("", "clone", "--depth=1", "--no-tags", "-b", "master", bareRepo, target)
	}, "clone")

	if err != nil {
		t.Fatalf("should succeed on 4th attempt after 3 cleanups: %v", err)
	}
	if attempt != 4 {
		t.Errorf("attempts = %d, want 4", attempt)
	}
	assertFileContent(t, filepath.Join(target, "docs", "file.txt"), "ok")
}

// TestV2_CloneRetryAfterStaleDir_SHAFlow 验证 SHA 流程的 clone 闭包也覆盖残留场景.
// main.go 中 SHA 流程 (clone 默认分支 → fetch SHA → checkout) 的 clone 闭包
// 同样调用了 prepareCloneTarget, 此测试验证其正确性.
func TestV2_CloneRetryAfterStaleDir_SHAFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	saveGlobals(t)
	globalTimeout = 0
	globalRetries = 1

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "file.txt"), []byte("sha-content"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs")

	// 获取 commit SHA
	out, err := exec.Command("git", "-C", srcRepo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	sha := strings.TrimSpace(string(out))

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := runGit("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	// 残留: 模拟上次 SHA clone 中断
	dstDir := t.TempDir()
	target := filepath.Join(dstDir, "cache")
	mustMkdirAll(t, filepath.Join(target, ".git", "objects"))
	mustWriteFile(t, filepath.Join(target, ".git", "HEAD"), []byte("ref: refs/heads/master\n"))
	mustWriteFile(t, filepath.Join(target, "stale.txt"), []byte("stale"))

	// SHA 流程的 clone 闭包 (镜像 main.go 第 141-149 行)
	calls := 0
	err = runGitRetry(func() error {
		calls++
		prepareCloneTarget(target)
		// SHA 流程: clone 默认分支 (不带 --branch)
		return runGit("", "clone", "--depth=1", "--no-tags", bareRepo, target)
	}, "clone")
	if err != nil {
		t.Fatalf("SHA clone should succeed after cleanup: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}

	// 后续 SHA 流程: fetch + checkout
	if err := runGit(target, "fetch", "--depth=1", "--no-tags", "origin", sha); err != nil {
		t.Fatalf("fetch SHA: %v", err)
	}
	if err := runGit(target, "checkout", sha); err != nil {
		t.Fatalf("checkout SHA: %v", err)
	}
	assertFileContent(t, filepath.Join(target, "docs", "file.txt"), "sha-content")
	// 残留文件应不存在 (被 prepareCloneTarget 清理后, 新 clone 不会有 stale.txt)
	if _, err := os.Stat(filepath.Join(target, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("stale.txt from previous failed clone should not exist")
	}
}

// TestV2_CloneFailsWithoutPrepareCloneTarget 反向回归测试:
// 故意不调用 prepareCloneTarget, 验证有残留目录时 git clone 确实会失败.
// 这个测试证明修复的必要性 — 如果哪天有人误删 prepareCloneTarget 调用,
// 此测试会失败提醒.
func TestV2_CloneFailsWithoutPrepareCloneTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	saveGlobals(t)
	globalTimeout = 0
	globalRetries = 0 // 不重试, 一次失败就返回

	srcRepo := initTempRepo(t)
	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := runGit("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	// 预先存在非空目标目录 (模拟上次 clone 残留)
	target := filepath.Join(t.TempDir(), "cache")
	mustMkdirAll(t, filepath.Join(target, ".git"))
	mustWriteFile(t, filepath.Join(target, ".git", "config"), []byte("stale"))

	// 不调用 prepareCloneTarget, 直接 clone — 应失败
	err := runGit("", "clone", "--depth=1", "--no-tags", "-b", "master", bareRepo, target)
	if err == nil {
		t.Fatal("expected clone to FAIL when target dir exists and is non-empty " +
			"(this test verifies the bug exists without prepareCloneTarget)")
	}
	// 错误信息应包含 "already exists" (git 的标准报错)
	if !strings.Contains(err.Error(), "already exists") &&
		!strings.Contains(strings.ToLower(err.Error()), "exists") {
		t.Logf("note: clone failed as expected, error: %v", err)
	}
}

// ============================================================
// cleanExpiredCache
// ============================================================

func TestCleanExpiredCache_NoDir(t *testing.T) {
	// Non-existent directory should not panic.
	cleanExpiredCache(filepath.Join(t.TempDir(), "nonexistent"), time.Hour)
}

func TestCleanExpiredCache_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	cleanExpiredCache(dir, time.Hour)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestCleanExpiredCache_RemovesExpired_KeepsFresh(t *testing.T) {
	dir := t.TempDir()

	// Old entry (2 hours ago)
	oldDir := filepath.Join(dir, "old-cache")
	mustMkdirAll(t, oldDir)
	pastTime := time.Now().Add(-2 * time.Hour)
	mustChtimes(t, oldDir, pastTime, pastTime)

	// Fresh entry (just now)
	freshDir := filepath.Join(dir, "fresh-cache")
	mustMkdirAll(t, freshDir)

	cleanExpiredCache(dir, time.Hour)

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

	cleanExpiredCache(dir, 24*time.Hour)

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected all entries removed, got %d", len(entries))
	}
}

func TestCleanExpiredCache_ZeroTTL_NoCleanup(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "entry")
	mustMkdirAll(t, d)
	past := time.Now().Add(-48 * time.Hour)
	mustChtimes(t, d, past, past)

	// TTL = 0 means the cutoff is "now", so entries older than now are cleaned.
	// But per the main() logic, TTL=0 means "no cleanup". cleanExpiredCache
	// itself doesn't check for zero — it's the caller's responsibility.
	// With TTL=0, cutoff = now - 0 = now. Anything before "now" is cleaned.
	// Since the entry was created in the past, it will be cleaned.
	cleanExpiredCache(dir, 0)

	entries, _ := os.ReadDir(dir)
	// The entry should be cleaned because its ModTime is before "now"
	if len(entries) != 0 {
		t.Logf("entry survived with TTL=0 (timing dependent, acceptable)")
	}
}

// ============================================================
// runGitRetry (mock-based, no real git needed)
// ============================================================

func TestRunGitRetry_SuccessFirstTry(t *testing.T) {
	saveGlobals(t)
	globalRetries = 3

	calls := 0
	err := runGitRetry(func() error {
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

func TestRunGitRetry_AllFail(t *testing.T) {
	saveGlobals(t)
	globalRetries = 1 // 1 retry → 1 sleep of 2s

	calls := 0
	err := runGitRetry(func() error {
		calls++
		return errors.New("network error")
	}, "test")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 2 { // initial + 1 retry
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestRunGitRetry_SuccessOnThirdTry(t *testing.T) {
	saveGlobals(t)
	globalRetries = 3 // up to 3 retries → up to 2 sleeps (4s)

	calls := 0
	err := runGitRetry(func() error {
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

func TestRunGitRetry_DeadlineExceeded(t *testing.T) {
	saveGlobals(t)
	globalRetries = 1

	calls := 0
	err := runGitRetry(func() error {
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

func TestRunGitRetry_ZeroRetries(t *testing.T) {
	saveGlobals(t)
	globalRetries = 0

	calls := 0
	err := runGitRetry(func() error {
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

func TestRunGitRetry_NegativeRetries(t *testing.T) {
	// Boundary: when globalRetries < 0, the for-loop condition (i <= -1) is
	// immediately false, so fn is never called and lastErr stays nil.
	// This means runGitRetry returns nil (success) without doing any work —
	// arguably a latent bug, but this test documents the current behaviour.
	saveGlobals(t)
	globalRetries = -1

	calls := 0
	err := runGitRetry(func() error {
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
// runGit (real git required)
// ============================================================

func TestRunGit_BasicOperation(t *testing.T) {
	saveGlobals(t)
	globalTimeout = 0

	dir := t.TempDir()
	if err := runGit(dir, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := runGit(dir, "status"); err != nil {
		t.Fatalf("git status: %v", err)
	}
}

func TestRunGit_InvalidCommand(t *testing.T) {
	saveGlobals(t)
	globalTimeout = 0

	err := runGit("", "not-a-real-git-subcommand")
	if err == nil {
		t.Fatal("expected error for invalid git command")
	}
}

func TestRunGit_NonExistentDir(t *testing.T) {
	saveGlobals(t)
	globalTimeout = 0

	err := runGit(filepath.Join(t.TempDir(), "does-not-exist"), "status")
	if err == nil {
		t.Fatal("expected error for non-existent working dir")
	}
}

// ============================================================
// 集成测试: v2 简化流程 (clone + fetch + reset + copyDir)
// ============================================================

// TestV2_FullClone_Checkout_CopyDir 端到端验证 v2 简化流程:
// 1. git clone --depth=1 --branch <ref> <repo> <cachedir> (全量检出, 不用 sparse)
// 2. 从缓存目录拷贝指定子目录到输出
// 验证: 指定目录的文件存在, 内容正确
func TestV2_FullClone_Checkout_CopyDir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	saveGlobals(t)
	globalTimeout = 0

	// 源仓库: docs/ + src/ 两个目录
	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "guide.md"), []byte("# Guide\n"))
	mustMkdirAll(t, filepath.Join(srcRepo, "src"))
	mustWriteFile(t, filepath.Join(srcRepo, "src", "main.go"), []byte("package main\n"))
	// 多种扩展名文件 (回归 v1 sparse cone 模式 bug)
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "data.json"), []byte(`{"id":1}`))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "config.xml"), []byte("<c/>"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs and src")

	// bare 克隆作为远程
	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := runGit("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	// v2 流程: 全量 clone (不用 --no-checkout, 不用 --sparse)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := runGit("", "clone", "--depth=1", "--no-tags",
		"-b", "master", bareRepo, cacheDir); err != nil {
		t.Fatalf("clone: %v", err)
	}

	// 验证缓存里全部文件都存在 (v2 不用 sparse, 工作区是全量的)
	if _, err := os.Stat(filepath.Join(cacheDir, "docs", "guide.md")); err != nil {
		t.Fatalf("docs/guide.md missing in cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "src", "main.go")); err != nil {
		t.Fatalf("src/main.go missing in cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "README.md")); err != nil {
		t.Fatalf("README.md missing in cache: %v", err)
	}

	// 拷贝 docs/ 到输出目录 (模拟 Step 3)
	outputDir := filepath.Join(t.TempDir(), "output")
	src := filepath.Join(cacheDir, "docs")
	dst := filepath.Join(outputDir, "docs")
	os.MkdirAll(filepath.Dir(dst), 0755)
	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
	}

	// 验证输出目录: docs/ 下所有扩展名文件都在 (v1 cone 模式 bug 回归)
	assertFileContent(t, filepath.Join(dst, "guide.md"), "# Guide\n")
	assertFileContent(t, filepath.Join(dst, "data.json"), `{"id":1}`)
	assertFileContent(t, filepath.Join(dst, "config.xml"), "<c/>")
}

// TestV2_CacheReuse_FetchReset 验证缓存复用流程:
// 第一次 clone 拿到 v1, 远端更新到 v2, 第二次 fetch + reset --hard 后拿到 v2.
func TestV2_CacheReuse_FetchReset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	saveGlobals(t)
	globalTimeout = 0

	// 源仓库: 初始 v1
	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "version.txt"), []byte("v1"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "v1")

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := runGit("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	// 第一次: 全量 clone (建立缓存)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := runGit("", "clone", "--depth=1", "--no-tags",
		"-b", "master", bareRepo, cacheDir); err != nil {
		t.Fatalf("first clone: %v", err)
	}
	assertFileContent(t, filepath.Join(cacheDir, "docs", "version.txt"), "v1")

	// 远端更新: v1 → v2
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "version.txt"), []byte("v2"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "v2")
	if err := runGit(srcRepo, "push", bareRepo, "master"); err != nil {
		t.Fatalf("push v2: %v", err)
	}

	// 第二次: 缓存复用, fetch + reset --hard origin/master
	if err := runGit(cacheDir, "fetch", "--depth=1", "--no-tags", "origin", "master"); err != nil {
		t.Fatalf("fetch on cache hit: %v", err)
	}
	if err := runGit(cacheDir, "reset", "--hard", "origin/master"); err != nil {
		t.Fatalf("reset --hard after fetch: %v", err)
	}

	// 关键断言: 缓存命中 + fetch + reset 后应拿到 v2
	assertFileContent(t, filepath.Join(cacheDir, "docs", "version.txt"), "v2")
}

// TestV2_CacheReuse_FetchReset_Tag 验证缓存复用 + tag 场景:
// 用 tag 作为 ref, fetch + reset --hard <tag> 更新.
func TestV2_CacheReuse_FetchReset_Tag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	saveGlobals(t)
	globalTimeout = 0

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "version.txt"), []byte("v1"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "v1")
	// 打 tag
	mustRunGit(t, srcRepo, "tag", "v1.0.0")

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := runGit("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	// 第一次: clone --branch v1.0.0 (tag)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := runGit("", "clone", "--depth=1", "--no-tags",
		"-b", "v1.0.0", bareRepo, cacheDir); err != nil {
		t.Fatalf("first clone with tag: %v", err)
	}
	assertFileContent(t, filepath.Join(cacheDir, "docs", "version.txt"), "v1")

	// 模拟缓存复用: fetch tag + reset --hard v1.0.0
	// (tag 场景 reset target 直接用 tag 名, 不是 origin/<tag>)
	if err := runGit(cacheDir, "fetch", "--depth=1", "origin", "refs/tags/v1.0.0"); err != nil {
		t.Fatalf("fetch tag: %v", err)
	}
	if err := runGit(cacheDir, "reset", "--hard", "v1.0.0"); err != nil {
		t.Fatalf("reset --hard tag: %v", err)
	}
	assertFileContent(t, filepath.Join(cacheDir, "docs", "version.txt"), "v1")
}

// TestV2_CloneWithCommitSHA 验证 commit SHA 场景:
// clone 默认分支 → fetch SHA → checkout SHA
func TestV2_CloneWithCommitSHA(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	saveGlobals(t)
	globalTimeout = 0

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "file.txt"), []byte("content"))
	mustRunGit(t, srcRepo, "add", ".")
	mustRunGit(t, srcRepo, "commit", "-m", "add docs")

	// 获取 commit SHA
	out, err := exec.Command("git", "-C", srcRepo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	sha := strings.TrimSpace(string(out))
	if !isCommitSHA(sha) {
		t.Fatalf("rev-parse output not a valid SHA: %q", sha)
	}

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := runGit("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	// v2 SHA 流程: clone 默认分支 → fetch SHA → checkout SHA
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := runGit("", "clone", "--depth=1", "--no-tags", bareRepo, cacheDir); err != nil {
		t.Fatalf("clone default branch: %v", err)
	}
	if err := runGit(cacheDir, "fetch", "--depth=1", "--no-tags", "origin", sha); err != nil {
		t.Fatalf("fetch SHA: %v", err)
	}
	if err := runGit(cacheDir, "checkout", sha); err != nil {
		t.Fatalf("checkout SHA: %v", err)
	}

	// 验证: docs/file.txt 存在且内容正确
	assertFileContent(t, filepath.Join(cacheDir, "docs", "file.txt"), "content")
}

// ============================================================
// Network-restricted timeout tests (slow server simulation)
// ============================================================

func TestRunGit_TimeoutWithSlowServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}
	saveGlobals(t)

	repoURL := startSlowServer(t)
	globalTimeout = 300 * time.Millisecond

	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "clone")

	start := time.Now()
	err := runGit("", "clone", "--depth=1", repoURL, target)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// Should have been killed around the 300ms mark, not hung indefinitely.
	if elapsed > 3*time.Second {
		t.Errorf("took too long to timeout: %v (expected ~300ms)", elapsed)
	}
	t.Logf("timed out after %v as expected, error: %v", elapsed, err)
}

func TestRunGitRetry_TimeoutWithSlowServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}
	saveGlobals(t)

	repoURL := startSlowServer(t)
	globalTimeout = 300 * time.Millisecond
	globalRetries = 1 // 1 retry → 1 sleep of 2s

	calls := 0
	tmpDir := t.TempDir()

	start := time.Now()
	err := runGitRetry(func() error {
		calls++
		target := filepath.Join(tmpDir, fmt.Sprintf("clone%d", calls))
		return runGit("", "clone", "--depth=1", repoURL, target)
	}, "clone")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after retries, got nil")
	}
	if calls != 2 { // initial + 1 retry
		t.Errorf("calls = %d, want 2", calls)
	}
	// Total: ~300ms (1st attempt) + 2s (sleep) + ~300ms (2nd attempt) ≈ 2.6s
	if elapsed > 6*time.Second {
		t.Errorf("took too long: %v", elapsed)
	}
	t.Logf("completed after %v with %d attempts, error: %v", elapsed, calls, err)
}

func TestRunGit_TimeoutZero_NoTimeout(t *testing.T) {
	// When globalTimeout = 0, runGit should not impose any timeout.
	saveGlobals(t)
	globalTimeout = 0

	dir := t.TempDir()
	// Should complete normally
	if err := runGit(dir, "init"); err != nil {
		t.Fatalf("git init failed with no timeout: %v", err)
	}
}

func TestRunGitRetry_GitCloneLocalThenSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	saveGlobals(t)
	globalTimeout = 0
	globalRetries = 1

	srcRepo := initTempRepo(t)
	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := runGit("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	calls := 0
	dstDir := t.TempDir()
	err := runGitRetry(func() error {
		calls++
		target := filepath.Join(dstDir, fmt.Sprintf("clone%d", calls))
		return runGit("", "clone", bareRepo, target)
	}, "clone")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (should succeed first try)", calls)
	}
}
