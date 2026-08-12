package puller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// ============================================================
// 测试用 mock Puller
// ============================================================

// mockPuller 可编程的 Puller 实现, 用于测试 fetchWithRetry.
// failBefore=N: 前 N 次 FetchRepo 调用返回 errFakeFail, 第 N+1 次返回 nil.
type mockPuller struct {
	name          string
	failBefore    int32
	calls         int32
	workDirExists []bool // 每次 FetchRepo 调用时 workDir 是否存在
}

func (m *mockPuller) Name() string { return m.name }
func (m *mockPuller) Desc() string { return "mock" }

func (m *mockPuller) FetchRepo(ctx context.Context, opts Options, workDir string, cached bool) error {
	cur := atomic.AddInt32(&m.calls, 1)
	_, err := os.Stat(workDir)
	m.workDirExists = append(m.workDirExists, err == nil)
	if int32(cur) <= m.failBefore {
		return errFakeFail
	}
	return nil
}

var errFakeFail = errors.New("fake fetch failure")

// ============================================================
// fetchWithRetry 整体重试
// ============================================================

// TestFetchWithRetry_NoRetry_FirstTrySuccess 验证首次成功不重试.
func TestFetchWithRetry_NoRetry_FirstTrySuccess(t *testing.T) {
	p := &mockPuller{name: "mock", failBefore: 0}
	opts := Options{FetchRetries: 1}
	cache := Cache{}

	err := fetchWithRetry(context.Background(), p, opts, "/nonexistent/path", cache)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&p.calls); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

// TestFetchWithRetry_Retry0_NoOverallRetry 验证 FetchRetries=0 时失败不整体重试.
func TestFetchWithRetry_Retry0_NoOverallRetry(t *testing.T) {
	p := &mockPuller{name: "mock", failBefore: 1}
	opts := Options{FetchRetries: 0}
	cache := Cache{}

	err := fetchWithRetry(context.Background(), p, opts, "/nonexistent/path", cache)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := atomic.LoadInt32(&p.calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no overall retry)", got)
	}
}

// TestFetchWithRetry_FailThenSuccess 验证首次失败 → 整体重试 → 成功.
func TestFetchWithRetry_FailThenSuccess(t *testing.T) {
	p := &mockPuller{name: "mock", failBefore: 1}
	opts := Options{FetchRetries: 1}
	cache := Cache{}

	err := fetchWithRetry(context.Background(), p, opts, "/nonexistent/path", cache)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&p.calls); got != 2 {
		t.Errorf("calls = %d, want 2 (1 fail + 1 success)", got)
	}
}

// TestFetchWithRetry_AllFail 验证所有重试用尽后返回最后一次错误.
func TestFetchWithRetry_AllFail(t *testing.T) {
	p := &mockPuller{name: "mock", failBefore: 99}
	opts := Options{FetchRetries: 2}
	cache := Cache{}

	err := fetchWithRetry(context.Background(), p, opts, "/nonexistent/path", cache)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := atomic.LoadInt32(&p.calls); got != 3 {
		t.Errorf("calls = %d, want 3 (1 initial + 2 retries)", got)
	}
}

// TestFetchWithRetry_CleansWorkDirBeforeRetry 验证整体重试前清理 workDir.
// 用真实文件系统: 首次 FetchRepo 在 workDir 写文件并失败, 整体重试前应删掉.
func TestFetchWithRetry_CleansWorkDirBeforeRetry(t *testing.T) {
	dir := t.TempDir()
	workDir := filepath.Join(dir, "work")

	wrapped := &writingMockPuller{
		mockPuller: mockPuller{name: "mock", failBefore: 1},
		workDir:    workDir,
	}
	opts := Options{FetchRetries: 1}
	cache := Cache{}

	err := fetchWithRetry(context.Background(), wrapped, opts, workDir, cache)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 验证整体重试前文件被清理 (writingMockPuller 第二次调用时应看不到残留)
	if wrapped.workDirExistsOnRetry {
		t.Error("workDir should be cleaned before retry, but files remained")
	}
}

// writingMockPuller 包装 mockPuller, 在失败时写文件, 在成功时检查文件是否被清理.
type writingMockPuller struct {
	mockPuller
	workDir              string
	workDirExistsOnRetry bool
}

func (m *writingMockPuller) FetchRepo(ctx context.Context, opts Options, workDir string, cached bool) error {
	cur := atomic.AddInt32(&m.calls, 1)
	if int32(cur) <= m.failBefore {
		// 失败前写文件, 模拟 fresh 中断残留
		os.MkdirAll(workDir, 0755)
		os.WriteFile(filepath.Join(workDir, "stale.txt"), []byte("stale"), 0644)
		return errFakeFail
	}
	// 成功那次: 检查残留文件是否被清理
	if _, err := os.Stat(filepath.Join(workDir, "stale.txt")); err == nil {
		m.workDirExistsOnRetry = true
	}
	return nil
}

// ============================================================
// 集成: Run 端到端 (用 mock Puller)
// ============================================================

// TestRun_FetchRetriesRecoversFromCorruptedCache 验证 Run 整体流程:
// FetchRepo 首次失败 (模拟缓存损坏) → 整体重试 → 成功 → LFS/拷贝跳过 → 完成.
func TestRun_FetchRetriesRecoversFromCorruptedCache(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	os.MkdirAll(outDir, 0755)

	// 模拟一个会写 workDir 文件的 Puller: 首次失败, 第二次成功 + 写一个目录供拷贝
	p := &endToEndMockPuller{
		mockPuller: mockPuller{name: "e2e", failBefore: 1},
		dirs:       []string{"docs"},
	}

	opts := Options{
		Repo:         "fake",
		Ref:          "main",
		Dirs:         []string{"docs"},
		Output:       outDir,
		CacheDir:     dir,
		Mode:         "e2e",
		FetchRetries: 1,
		NoLFS:        true,
	}

	err := Run(p, opts)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got := atomic.LoadInt32(&p.calls); got < 2 {
		t.Errorf("FetchRepo calls = %d, want >= 2 (retry after failure)", got)
	}
	// 验证拷贝到输出
	if _, err := os.Stat(filepath.Join(outDir, "docs", "file.txt")); err != nil {
		t.Errorf("output file missing: %v", err)
	}
}

// endToEndMockPuller 端到端测试用: FetchRepo 成功时在 workDir/docs 写文件.
type endToEndMockPuller struct {
	mockPuller
	dirs []string
}

func (m *endToEndMockPuller) FetchRepo(ctx context.Context, opts Options, workDir string, cached bool) error {
	cur := atomic.AddInt32(&m.calls, 1)
	if int32(cur) <= m.failBefore {
		return errFakeFail
	}
	// 成功: 写一个 docs/file.txt 到 workDir, 供 CopyDirsToOutput 拷贝
	os.MkdirAll(filepath.Join(workDir, "docs"), 0755)
	return os.WriteFile(filepath.Join(workDir, "docs", "file.txt"), []byte("ok"), 0644)
}
