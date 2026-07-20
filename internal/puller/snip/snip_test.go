package snip

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
// Test helpers (mirror fullpull_test.go for self-contained integration tests)
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

// skipIfLowGit 在 Git < 2.25 (不支持 sparse-checkout --cone) 时跳过测试.
// snip 模式端到端测试需要 cone 支持; 低版本环境只跑 TestPull_LowGitVersion_Snip.
func skipIfLowGit(t *testing.T) {
	t.Helper()
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		t.Fatalf("git --version: %v", err)
	}
	ver, err := gitutil.ParseGitVersion(string(out))
	if err != nil {
		t.Fatalf("ParseGitVersion: %v", err)
	}
	if !gitutil.SupportsSparseCheckoutCone(ver) {
		t.Skipf("跳过: git %d.%d.%d 不支持 sparse-checkout --cone (需 2.25+)",
			ver[0], ver[1], ver[2])
	}
}

// ============================================================
// Puller registration / metadata
// ============================================================

// TestPuller_Name 验证 snip 模式名称正确.
func TestPuller_Name(t *testing.T) {
	p := &Puller{}
	if got := p.Name(); got != "snip" {
		t.Errorf("Name() = %q, want %q", got, "snip")
	}
}

// TestPuller_Registered 验证 snip 模式已注册到 puller 注册表.
func TestPuller_Registered(t *testing.T) {
	p, err := puller.Get("snip")
	if err != nil {
		t.Fatalf("Get(\"snip\"): %v", err)
	}
	if p.Name() != "snip" {
		t.Errorf("registered puller Name = %q, want \"snip\"", p.Name())
	}
	if p.Desc() == "" {
		t.Error("Desc() should not be empty")
	}
}

// TestAvailableModes_ContainsSnip 验证 AvailableModes 包含 snip.
func TestAvailableModes_ContainsSnip(t *testing.T) {
	modes := puller.AvailableModes()
	if !strings.Contains(modes, "snip") {
		t.Errorf("AvailableModes() = %q, want to contain \"snip\"", modes)
	}
}

// ============================================================
// Pull() 端到端集成测试
// ============================================================

// TestPull_SnipFlow_Branch 验证 snip 模式完整流程:
// init repo → bare → Pull(branch) → Output/<dir> 文件正确,
// 且工作区只检出指定目录 (sparse-checkout 生效).
func TestPull_SnipFlow_Branch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	skipIfLowGit(t)
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
	workDir := filepath.Join(cacheDir, gitutil.CacheHash(bareRepo, "master"))
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err != nil {
		t.Errorf("cache .git not found: %v", err)
	}

	// 验证 sparse-checkout 生效: 工作区只有 docs/, 没有 src/
	if _, err := os.Stat(filepath.Join(workDir, "src")); !os.IsNotExist(err) {
		t.Errorf("sparse-checkout 未生效: src/ 不应存在于工作区 (err=%v)", err)
	}
}

// TestPull_CacheReuse_Snip 验证 snip 模式缓存命中: 第二次 Pull 同 repo+ref 走 fetch+reset 路径拿到最新版本.
func TestPull_CacheReuse_Snip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	skipIfLowGit(t)
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

	// 第一次: 全新 sparse fetch
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

// TestPull_CommitSHA_Snip 验证 snip 模式 commit SHA 流程: fetch SHA → checkout FETCH_HEAD.
func TestPull_CommitSHA_Snip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	skipIfLowGit(t)
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

// TestPull_MultiDirs_Snip 验证 snip 模式多目录拉取: sparse-checkout set <dir1> <dir2>.
func TestPull_MultiDirs_Snip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	skipIfLowGit(t)
	r := &gitutil.Runner{}

	srcRepo := initTempRepo(t)
	mustMkdirAll(t, filepath.Join(srcRepo, "docs"))
	mustWriteFile(t, filepath.Join(srcRepo, "docs", "a.txt"), []byte("aaa"))
	mustMkdirAll(t, filepath.Join(srcRepo, "src"))
	mustWriteFile(t, filepath.Join(srcRepo, "src", "b.txt"), []byte("bbb"))
	mustMkdirAll(t, filepath.Join(srcRepo, "test"))
	mustWriteFile(t, filepath.Join(srcRepo, "test", "c.txt"), []byte("ccc"))
	mustRunGit(t, r, srcRepo, "add", ".")
	mustRunGit(t, r, srcRepo, "commit", "-m", "add docs, src, test")

	bareRepo := filepath.Join(t.TempDir(), "bare.git")
	if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
		t.Fatalf("clone --bare: %v", err)
	}

	cacheDir := t.TempDir()
	outputDir := t.TempDir()
	p := &Puller{}
	if err := puller.Run(p, puller.Options{
		Repo: bareRepo, Ref: "master", Dirs: []string{"docs", "src"},
		Output: outputDir, CacheDir: cacheDir, NoCache: true, NoLFS: true, Runner: r,
	}); err != nil {
		t.Fatalf("Pull multi-dirs: %v", err)
	}

	// 两个指定目录应存在
	assertFileContent(t, filepath.Join(outputDir, "docs", "a.txt"), "aaa")
	assertFileContent(t, filepath.Join(outputDir, "src", "b.txt"), "bbb")

	// 未指定的 test 目录不应存在于工作区
	workDir := filepath.Join(cacheDir, gitutil.CacheHash(bareRepo, "master"))
	if _, err := os.Stat(filepath.Join(workDir, "test")); !os.IsNotExist(err) {
		t.Errorf("sparse-checkout 未生效: test/ 不应存在于工作区 (err=%v)", err)
	}
}

// TestPull_MissingDir_Snip 验证 snip 模式指定目录不存在时 Pull 返回错误.
func TestPull_MissingDir_Snip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	skipIfLowGit(t)
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

// TestPull_LowGitVersion_Snip 验证 snip 模式在低版本 Git (< 2.25) 下返回清晰错误,
// 而非执行 sparse-checkout 命令时崩溃.
//
// 此测试在宿主机高版本 Git 上验证 "支持 cone 时不报错";
// 在 Docker 低版本 Git 容器内 (GITSPARSE_TEST_LOW_GIT=1) 验证 "不支持时返回引导错误".
func TestPull_LowGitVersion_Snip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := &Puller{}

	// 检测当前环境的 Git 版本
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		t.Fatalf("git --version: %v", err)
	}
	ver, err := gitutil.ParseGitVersion(string(out))
	if err != nil {
		t.Fatalf("ParseGitVersion: %v", err)
	}
	supported := gitutil.SupportsSparseCheckoutCone(ver)

	if os.Getenv("GITSPARSE_TEST_LOW_GIT") == "1" {
		// 低版本环境: snip 应返回清晰错误, 引导用户用 full 模式
		if supported {
			t.Fatalf("环境变量 GITSPARSE_TEST_LOW_GIT=1 但 git %d.%d.%d 支持 cone, 测试配置矛盾",
				ver[0], ver[1], ver[2])
		}
		srcRepo := initTempRepo(t)
		r := &gitutil.Runner{}
		bareRepo := filepath.Join(t.TempDir(), "bare.git")
		if err := r.Run("", "clone", "--bare", srcRepo, bareRepo); err != nil {
			t.Fatalf("clone --bare: %v", err)
		}
		err := puller.Run(p, puller.Options{
			Repo: bareRepo, Ref: "master", Dirs: []string{"docs"},
			Output: t.TempDir(), CacheDir: t.TempDir(),
			NoCache: true, NoLFS: true, Runner: r,
		})
		if err == nil {
			t.Fatal("低版本 Git 下 snip 应返回错误, got nil")
		}
		if !strings.Contains(err.Error(), "full") {
			t.Errorf("错误应引导用户使用 full 模式, got: %v", err)
		}
		t.Logf("低版本 Git (%d.%d.%d) 正确返回错误: %v", ver[0], ver[1], ver[2], err)
		return
	}

	// 高版本环境 (宿主机): 版本检测应通过, 不报错
	if !supported {
		t.Fatalf("宿主机 git %d.%d.%d 不支持 cone, 请在 Docker 中运行低版本测试",
			ver[0], ver[1], ver[2])
	}
	// checkGitVersion 不应返回错误
	if err := p.checkGitVersion(); err != nil {
		t.Errorf("高版本 Git 下 checkGitVersion 不应报错: %v", err)
	}
}
