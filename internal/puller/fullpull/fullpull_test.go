package fullpull

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/workmule/gitsparse/internal/gitutil"
	"github.com/workmule/gitsparse/internal/puller"
)

// ============================================================
// Test helpers (mirror gitutil_test.go for self-contained integration tests)
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

func initTempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	r := &gitutil.Runner{}
	mustRunGit(t, r, dir, "init")
	mustRunGit(t, r, dir, "config", "user.email", "test@test.com")
	mustRunGit(t, r, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustRunGit(t, r, dir, "add", ".")
	mustRunGit(t, r, dir, "commit", "-m", "init")
	return dir
}

func mustRunGit(t *testing.T, r *gitutil.Runner, dir string, args ...string) {
	t.Helper()
	if err := r.Run(dir, args...); err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
}

// ============================================================
// Puller registration / metadata
// ============================================================

func TestPuller_Name(t *testing.T) {
	p := &Puller{}
	if p.Name() != "full" {
		t.Errorf("Name() = %q, want %q", p.Name(), "full")
	}
}

func TestPuller_Registered(t *testing.T) {
	p, err := puller.Get("full")
	if err != nil {
		t.Fatalf("Get(full): %v", err)
	}
	if p.Name() != "full" {
		t.Errorf("registered puller Name = %q, want full", p.Name())
	}
	if p.Desc() == "" {
		t.Error("Desc() should not be empty")
	}
}

func TestAvailableModes_ContainsFull(t *testing.T) {
	modes := puller.AvailableModes()
	if !strings.Contains(modes, "full") {
		t.Errorf("AvailableModes() = %q, want to contain 'full'", modes)
	}
}

// ============================================================
// Pull() 端到端集成测试
// ============================================================

// TestPull_FullFlow_Branch 验证全量拉取模式完整流程:
// init repo → bare → Pull(branch) → Output/<dir> 文件正确.
func TestPull_FullFlow_Branch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := &gitutil.Runner{}

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "guide.md"), []byte("# Guide\n"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "data.json"), []byte(`{"id":1}`))
	mustMkdirAll(t, filepath.Join(srcRepo, "src"))
	mustWriteFile(t, filepath.Join(srcRepo, "src", "main.go"), []byte("package main\n"))
	mustRunGit(t, r, srcRepo, "add", ".")
	mustRunGit(t, r, srcRepo, "commit", "-m", "add docs and src")

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	cacheDir := t.TempDir()
	outputDir := t.TempDir()

	p := &Puller{}
	err := puller.Run(p, puller.Options{
		Repo:     bareRepo,
		Ref:      "master",
		Dirs:     []string{"docs"},
		Output:   outputDir,
		CacheDir: cacheDir,
		NoCache:  true,
		NoLFS:    true,
		Runner:   r,
	})
	if err != nil {
		t.Fatalf("Pull failed: %v", err)
	}

	// 验证输出目录内容
	assertFileContent(t, filepath.Join(outputDir, "docs", "guide.md"), "# Guide\n")
	assertFileContent(t, filepath.Join(outputDir, "docs", "data.json"), `{"id":1}`)

	// 验证缓存目录已建立 (.git 存在)
	if _, err := os.Stat(filepath.Join(cacheDir, gitutil.CacheHash(bareRepo, "master"), ".git")); err != nil {
		t.Errorf("cache .git not found: %v", err)
	}
}

// TestPull_CacheReuse 验证缓存命中: 第二次 Pull 同 repo+ref 走 fetch+reset 路径.
func TestPull_CacheReuse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := &gitutil.Runner{}

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "version.txt"), []byte("v1"))
	mustRunGit(t, r, srcRepo, "add", ".")
	mustRunGit(t, r, srcRepo, "commit", "-m", "v1")

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	cacheDir := t.TempDir()
	p := &Puller{}

	// 第一次: 全新 clone
	out1 := t.TempDir()
	if err := puller.Run(p, puller.Options{
		Repo: bareRepo, Ref: "master", Dirs: []string{"docs"},
		Output: out1, CacheDir: cacheDir, NoLFS: true, Runner: r,
	}); err != nil {
		t.Fatalf("first Pull: %v", err)
	}
	assertFileContent(t, filepath.Join(out1, "docs", "version.txt"), "v1")

	// 远端更新到 v2
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "version.txt"), []byte("v2"))
	mustRunGit(t, r, srcRepo, "add", ".")
	mustRunGit(t, r, srcRepo, "commit", "-m", "v2")
	if err := r.Run(srcRepo, "push", bareRepo, "master"); err != nil {
		t.Fatalf("push v2: %v", err)
	}

	// 第二次: 缓存命中, 应走 fetch + reset 路径拿到 v2
	out2 := t.TempDir()
	if err := puller.Run(p, puller.Options{
		Repo: bareRepo, Ref: "master", Dirs: []string{"docs"},
		Output: out2, CacheDir: cacheDir, NoLFS: true, Runner: r,
	}); err != nil {
		t.Fatalf("second Pull (cache reuse): %v", err)
	}
	assertFileContent(t, filepath.Join(out2, "docs", "version.txt"), "v2")
}

// TestPull_CommitSHA 验证 commit SHA 流程: clone 默认分支 → fetch SHA → checkout.
func TestPull_CommitSHA(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := &gitutil.Runner{}

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "file.txt"), []byte("sha-content"))
	mustRunGit(t, r, srcRepo, "add", ".")
	mustRunGit(t, r, srcRepo, "commit", "-m", "add docs")

	out, err := exec.Command("git", "-C", srcRepo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	sha := strings.TrimSpace(string(out))

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	cacheDir := t.TempDir()
	outputDir := t.TempDir()
	p := &Puller{}
	if err := puller.Run(p, puller.Options{
		Repo: bareRepo, Ref: sha, Dirs: []string{"docs"},
		Output: outputDir, CacheDir: cacheDir, NoCache: true, NoLFS: true, Runner: r,
	}); err != nil {
		t.Fatalf("Pull SHA: %v", err)
	}
	assertFileContent(t, filepath.Join(outputDir, "docs", "file.txt"), "sha-content")
}

// TestPull_MissingDir 验证指定目录不存在时 Pull 返回错误.
func TestPull_MissingDir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r := &gitutil.Runner{}

	srcRepo := initTempRepo(t)
	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	cacheDir := t.TempDir()
	outputDir := t.TempDir()
	p := &Puller{}
	err := puller.Run(p, puller.Options{
		Repo: bareRepo, Ref: "master", Dirs: []string{"nonexistent"},
		Output: outputDir, CacheDir: cacheDir, NoCache: true, NoLFS: true, Runner: r,
	})
	if err == nil {
		t.Fatal("expected error for missing dir, got nil")
	}
}
