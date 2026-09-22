package puller

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// ============================================================
// Test helpers
// ============================================================

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustChtimes(t *testing.T, path string, atime, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, atime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// ============================================================
// Cache.Hit
// ============================================================

// TestCache_Hit_NoCacheDisabled 验证 NoCache=true 时永远返回 false.
func TestCache_Hit_NoCacheDisabled(t *testing.T) {
	dir := t.TempDir()
	// 预先建好 .git 目录
	mustMkdirAll(t, filepath.Join(dir, "sub", ".git"))

	c := Cache{NoCache: true}
	if c.Hit(filepath.Join(dir, "sub")) {
		t.Error("NoCache=true should always miss")
	}
}

// TestCache_Hit_NoGitDir 验证无 .git 目录时返回 false.
func TestCache_Hit_NoGitDir(t *testing.T) {
	dir := t.TempDir()
	c := Cache{}
	if c.Hit(dir) {
		t.Error("should miss when .git dir absent")
	}
}

// TestCache_Hit_GitDirExists 验证有效 git 仓库 (git init 后) 时返回 true.
func TestCache_Hit_GitDirExists(t *testing.T) {
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	c := Cache{}
	if !c.Hit(dir) {
		t.Error("should hit when dir is a valid git repo")
	}
}

// TestCache_Hit_CorruptedGitDir 验证 .git 目录存在但残缺 (无 HEAD/config) 时返回 false.
// 复现: fresh 流程 git init 中断留下空 .git 目录, 下次 Hit 不应误判命中.
func TestCache_Hit_CorruptedGitDir(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, ".git"))
	c := Cache{}
	if c.Hit(dir) {
		t.Error("should miss when .git exists but is not a valid repo")
	}
}

// TestCache_Hit_GitFileNotDir 验证 .git 是文件 (非目录, worktree 指向) 时不被误判.
// 注: 当前实现依赖 git rev-parse 判定, .git 文件指向的 worktree 同样算有效仓库;
// 此用例 .git 文件内容非真实 worktree 指向, 应返回 false.
func TestCache_Hit_GitFileNotDir(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".git"), []byte("gitdir: ..."))
	c := Cache{}
	if c.Hit(dir) {
		t.Error("should miss when .git is a file pointing nowhere")
	}
}

// ============================================================
// Cache.CleanShallowLock
// ============================================================

// TestCache_CleanShallowLock_Removes 验证存在 shallow.lock 时被删除.
func TestCache_CleanShallowLock_Removes(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, ".git"))
	lockPath := filepath.Join(dir, ".git", "shallow.lock")
	mustWriteFile(t, lockPath, []byte("stale"))

	c := Cache{}
	c.CleanShallowLock(dir)

	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("shallow.lock should be removed, got err=%v", err)
	}
}

// TestCache_CleanShallowLock_NoLock 验证无 shallow.lock 时不报错.
func TestCache_CleanShallowLock_NoLock(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, ".git"))
	c := Cache{}
	c.CleanShallowLock(dir) // 不应 panic
}

// TestCache_CleanShallowLock_NoGitDir 验证无 .git 目录时不报错.
func TestCache_CleanShallowLock_NoGitDir(t *testing.T) {
	dir := t.TempDir()
	c := Cache{}
	c.CleanShallowLock(dir) // 不应 panic
}

// ============================================================
// Cache.CleanExpired
// ============================================================

func TestCache_CleanExpired_NoDir(t *testing.T) {
	c := Cache{}
	c.CleanExpired(filepath.Join(t.TempDir(), "nonexistent"), time.Hour)
}

func TestCache_CleanExpired_RemovesExpired_KeepsFresh(t *testing.T) {
	root := t.TempDir()

	oldDir := filepath.Join(root, "old")
	mustMkdirAll(t, oldDir)
	past := time.Now().Add(-2 * time.Hour)
	mustChtimes(t, oldDir, past, past)

	freshDir := filepath.Join(root, "fresh")
	mustMkdirAll(t, freshDir)

	c := Cache{}
	c.CleanExpired(root, time.Hour)

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Error("old entry should be removed")
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Error("fresh entry should remain")
	}
}

func TestCache_CleanExpired_AllExpired(t *testing.T) {
	root := t.TempDir()
	for i := range 3 {
		d := filepath.Join(root, "c"+string(rune('0'+i)))
		mustMkdirAll(t, d)
		past := time.Now().Add(-48 * time.Hour)
		mustChtimes(t, d, past, past)
	}
	c := Cache{}
	c.CleanExpired(root, 24*time.Hour)
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("expected all removed, got %d", len(entries))
	}
}

func TestCache_CleanExpired_ZeroTTL_NoCleanup(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "entry")
	mustMkdirAll(t, d)
	c := Cache{}
	c.CleanExpired(root, 0) // 0 = 不清理
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Errorf("expected entry kept with TTL=0, got %d", len(entries))
	}
}

// ============================================================
// CopyDirsToOutput
// ============================================================

// ============================================================
// CopyFilesToOutput
// ============================================================

// TestCopyFilesToOutput_Glob 验证 glob 只拷贝匹配文件, 未匹配文件不拷贝.
func TestCopyFilesToOutput_Glob(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.MkdirAll(filepath.Join(src, "common/protocol"), 0755)
	os.WriteFile(filepath.Join(src, "common/protocol/a.xml"), []byte("xml"), 0644)
	os.WriteFile(filepath.Join(src, "common/protocol/b.json"), []byte("json"), 0644)

	out := filepath.Join(dir, "out")
	err := CopyFilesToOutput(src, out, []string{"common/protocol/*.xml"}, nil, false)
	if err != nil {
		t.Fatalf("CopyFilesToOutput: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "common/protocol/a.xml")); err != nil {
		t.Errorf("匹配文件未拷贝: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "common/protocol/b.json")); err == nil {
		t.Errorf("未匹配文件不应被拷贝")
	}
}

// TestCopyFilesToOutput_MultiGlobs 验证多 pattern 组合拷贝 (如 *.xml + *.json).
func TestCopyFilesToOutput_MultiGlobs(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.MkdirAll(filepath.Join(src, "proto"), 0755)
	os.WriteFile(filepath.Join(src, "proto/a.xml"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(src, "proto/b.json"), []byte("j"), 0644)
	os.WriteFile(filepath.Join(src, "proto/c.txt"), []byte("t"), 0644)

	out := filepath.Join(dir, "out")
	pats := []string{"proto/*.xml", "proto/*.json"}
	if err := CopyFilesToOutput(src, out, pats, nil, false); err != nil {
		t.Fatalf("CopyFilesToOutput: %v", err)
	}
	for _, f := range []string{"proto/a.xml", "proto/b.json"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("缺少 %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "proto/c.txt")); err == nil {
		t.Errorf("c.txt 不应被拷贝")
	}
}

// TestCopyFilesToOutput_NoMatch 验证 0 匹配: skipMissing=false 报错, true 跳过.
func TestCopyFilesToOutput_NoMatch(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.MkdirAll(src, 0755)

	if err := CopyFilesToOutput(src, dir, []string{"nope/*.xml"}, nil, false); err == nil {
		t.Errorf("0 匹配且 skipMissing=false 应报错")
	}
	if err := CopyFilesToOutput(src, dir, []string{"nope/*.xml"}, nil, true); err != nil {
		t.Errorf("0 匹配且 skipMissing=true 应跳过: %v", err)
	}
}

// TestCopyFilesToOutput_SkipUnderDirs 验证匹配项位于 -dirs 目录内时跳过 (整目录拷贝已覆盖).
func TestCopyFilesToOutput_SkipUnderDirs(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.MkdirAll(filepath.Join(src, "common/protocol"), 0755)
	os.WriteFile(filepath.Join(src, "common/protocol/a.xml"), []byte("x"), 0644)

	out := filepath.Join(dir, "out")
	err := CopyFilesToOutput(src, out, []string{"common/protocol/*.xml"}, []string{"common"}, false)
	if err != nil {
		t.Fatalf("CopyFilesToOutput: %v", err)
	}
	// 跳过 = 不由 CopyFilesToOutput 拷贝 (该场景下 dirs 拷贝由 CopyDirsToOutput 负责)
	if _, err := os.Stat(filepath.Join(out, "common/protocol/a.xml")); err == nil {
		t.Errorf("位于 -dirs 内的匹配项应跳过")
	}
}

// TestCopyFilesToOutput_DirMatch 验证 glob 匹配到目录时整目录拷贝.
func TestCopyFilesToOutput_DirMatch(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.MkdirAll(filepath.Join(src, "pkg/tool"), 0755)
	os.WriteFile(filepath.Join(src, "pkg/tool/a.go"), []byte("go"), 0644)

	out := filepath.Join(dir, "out")
	if err := CopyFilesToOutput(src, out, []string{"pkg/*"}, nil, false); err != nil {
		t.Fatalf("CopyFilesToOutput: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "pkg/tool/a.go")); err != nil {
		t.Errorf("目录匹配未整拷: %v", err)
	}
}

// TestCopyDirsToOutput_Basic 验证把 srcRoot 下的多个目录拷贝到 output.
func TestCopyDirsToOutput_Basic(t *testing.T) {
	srcRoot := t.TempDir()
	mustMkdirAll(t, filepath.Join(srcRoot, "docs"))
	mustWriteFile(t, filepath.Join(srcRoot, "docs", "a.txt"), []byte("hello"))
	mustMkdirAll(t, filepath.Join(srcRoot, "src"))
	mustWriteFile(t, filepath.Join(srcRoot, "src", "main.go"), []byte("package main"))

	output := t.TempDir()
	err := CopyDirsToOutput(srcRoot, output, []string{"docs", "src"}, false)
	if err != nil {
		t.Fatalf("CopyDirsToOutput: %v", err)
	}

	assertContent := func(path, want string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(data) != want {
			t.Errorf("%s = %q, want %q", path, data, want)
		}
	}
	assertContent(filepath.Join(output, "docs", "a.txt"), "hello")
	assertContent(filepath.Join(output, "src", "main.go"), "package main")
}

// TestCopyDirsToOutput_MissingDir 验证源目录不存在时返回错误.
func TestCopyDirsToOutput_MissingDir(t *testing.T) {
	srcRoot := t.TempDir()
	output := t.TempDir()
	err := CopyDirsToOutput(srcRoot, output, []string{"nonexistent"}, false)
	if err == nil {
		t.Fatal("expected error for missing dir")
	}
}

// TestCopyDirsToOutput_SkipMissingDir 验证 skipMissing=true 时跳过不存在目录, 其余正常拷贝.
func TestCopyDirsToOutput_SkipMissingDir(t *testing.T) {
	srcRoot := t.TempDir()
	mustMkdirAll(t, filepath.Join(srcRoot, "docs"))
	mustWriteFile(t, filepath.Join(srcRoot, "docs", "a.txt"), []byte("hello"))

	output := t.TempDir()
	err := CopyDirsToOutput(srcRoot, output, []string{"docs", "nonexistent"}, true)
	if err != nil {
		t.Fatalf("CopyDirsToOutput: %v", err)
	}
	if _, err := os.Stat(filepath.Join(output, "nonexistent")); !os.IsNotExist(err) {
		t.Errorf("nonexistent should not be created in output, got err=%v", err)
	}
	data, err := os.ReadFile(filepath.Join(output, "docs", "a.txt"))
	if err != nil || string(data) != "hello" {
		t.Errorf("docs/a.txt not copied correctly: data=%q err=%v", data, err)
	}
}

// TestCopyDirsToOutput_OverwritesExisting 验证目标已存在时先删后拷.
func TestCopyDirsToOutput_OverwritesExisting(t *testing.T) {
	srcRoot := t.TempDir()
	mustMkdirAll(t, filepath.Join(srcRoot, "docs"))
	mustWriteFile(t, filepath.Join(srcRoot, "docs", "new.txt"), []byte("new"))

	output := t.TempDir()
	mustMkdirAll(t, filepath.Join(output, "docs"))
	mustWriteFile(t, filepath.Join(output, "docs", "old.txt"), []byte("old"))

	if err := CopyDirsToOutput(srcRoot, output, []string{"docs"}, false); err != nil {
		t.Fatalf("CopyDirsToOutput: %v", err)
	}

	// old.txt 应被删除 (整个 docs 被 RemoveAll 后重建)
	if _, err := os.Stat(filepath.Join(output, "docs", "old.txt")); !os.IsNotExist(err) {
		t.Errorf("old.txt should be removed, got err=%v", err)
	}
	data, err := os.ReadFile(filepath.Join(output, "docs", "new.txt"))
	if err != nil {
		t.Fatalf("read new.txt: %v", err)
	}
	if string(data) != "new" {
		t.Errorf("new.txt = %q, want %q", data, "new")
	}
}
