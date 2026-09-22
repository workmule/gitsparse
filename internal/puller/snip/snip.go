// Package snip 实现"局部拉取"模式: sparse-checkout (cone) 仅检出指定目录.
//
// 借鉴 gitsnip (github.com/dagimg-dot/gitsnip) 的 sparse-checkout 算法:
//
//	git init → remote add → sparse-checkout init --cone → set <dirs>
//	→ fetch --depth=1 → checkout FETCH_HEAD
//
// 相比 gitsnip 的增强:
//   - 多目录支持 (sparse-checkout set <dir1> <dir2> ...)
//   - commit SHA 支持
//   - 缓存复用 (fetch + reset, 非每次临时目录)
//   - LFS 支持 (由 puller.Run 通用流程处理)
//   - 超时 + 重试 (由 gitutil.Runner 提供)
package snip

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/workmule/gitsparse/internal/gitutil"
	"github.com/workmule/gitsparse/internal/puller"
)

// ============================================================================
// 注册
// ============================================================================

func init() {
	puller.Register(&Puller{})
}

// Puller 局部拉取模式实现.
type Puller struct{}

// Name 模式名.
func (p *Puller) Name() string { return "snip" }

// Desc 模式简短描述.
func (p *Puller) Desc() string {
	return "局部拉取: sparse-checkout (cone) 仅检出指定目录 (Git 2.25+)"
}

// FetchRepo 把仓库拉到 workDir.
// cached=true 走增量更新 (sparse reapply+fetch+reset); false 走全新 sparse fetch.
func (p *Puller) FetchRepo(ctx context.Context, opts puller.Options, workDir string, cached bool) error {
	// Git 版本检测: snip 模式需要 sparse-checkout --cone (Git 2.25+)
	if err := p.checkGitVersion(); err != nil {
		return err
	}
	if !cached {
		return p.freshSparseFetch(ctx, opts, workDir)
	}
	return p.cacheSparseUpdate(ctx, opts, workDir)
}

// checkGitVersion 检测系统 Git 版本是否支持 sparse-checkout --cone (Git 2.25+).
// 不满足时返回清晰错误, 引导用户使用 full 模式.
func (p *Puller) checkGitVersion() error {
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return fmt.Errorf("snip: 无法检测 git 版本: %w", err)
	}
	ver, err := gitutil.ParseGitVersion(string(out))
	if err != nil {
		return fmt.Errorf("snip: 无法解析 git 版本 %q: %w", strings.TrimSpace(string(out)), err)
	}
	if !gitutil.SupportsSparseCheckoutCone(ver) {
		return fmt.Errorf("snip 模式需要 Git 2.25+ (当前 %d.%d.%d), 请使用 -mode full 代替",
			ver[0], ver[1], ver[2])
	}
	return nil
}

// freshSparseFetch 全新 sparse 拉取.
func (p *Puller) freshSparseFetch(ctx context.Context, opts puller.Options, workDir string) error {
	r := opts.RunnerOrNew()
	gitutil.Logf("Step 1: git init + sparse-checkout (cone)")
	t0 := time.Now()

	gitutil.PrepareCloneTarget(workDir)
	// git init 需要目标目录已存在 (full 模式的 git clone 会自动创建, init 不会)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return err
	}

	// 1. init + remote
	if err := r.RunWithTimeout(ctx, opts.Timeout, workDir, "init"); err != nil {
		return err
	}
	if err := r.RunWithTimeout(ctx, opts.Timeout, workDir, "remote", "add", "origin", opts.Repo); err != nil {
		return err
	}

	// 2. sparse-checkout init --cone + set <dirs>
	// cone 只认目录: -files 的 glob pattern 归约为父目录 (PatternDirs);
	// 全部为根级 pattern 时 sparseDirs 为空, 跳过 set (cone 默认检出根文件).
	if err := r.RunWithTimeout(ctx, opts.Timeout, workDir, "sparse-checkout", "init", "--cone"); err != nil {
		return err
	}
	sparseDirs := append(opts.Dirs, gitutil.PatternDirs(opts.Files)...)
	if len(sparseDirs) > 0 {
		setArgs := append([]string{"sparse-checkout", "set"}, sparseDirs...)
		if err := r.RunWithTimeout(ctx, opts.Timeout, workDir, setArgs...); err != nil {
			return err
		}
	}

	// 3. fetch --depth=1
	// attempt 0=首次, 1=第一次重试(直接重试, 假设瞬时抖动), >=2=第二次起清理残留 shallow.lock.
	if err := r.RunRetry(ctx, opts.Retries, func(ctx context.Context, attempt int) error {
		if attempt >= 2 {
			puller.Cache{}.CleanShallowLock(workDir)
		}
		return r.RunWithTimeout(ctx, opts.Timeout, workDir, "fetch", "--depth=1", "--no-tags",
			"origin", opts.Ref)
	}, "fetch"); err != nil {
		return err
	}

	// 4. checkout FETCH_HEAD (被 kill 会留 index.lock, 清锁让整体重试能跑 fresh).
	if err := r.RunWithTimeout(ctx, opts.Timeout, workDir, "checkout", "FETCH_HEAD"); err != nil {
		puller.Cache{}.CleanGitLocks(workDir)
		return err
	}

	gitutil.Logf("Step 1 完成 (%s)", time.Since(t0))
	return nil
}

// cacheSparseUpdate 缓存复用: reapply sparse + fetch + reset.
func (p *Puller) cacheSparseUpdate(ctx context.Context, opts puller.Options, workDir string) error {
	r := opts.RunnerOrNew()
	gitutil.Logf("Step 1: 缓存复用, sparse-checkout 更新")
	t0 := time.Now()

	cache := puller.Cache{NoCache: opts.NoCache}
	cache.CleanShallowLock(workDir)

	// sparse-checkout set (dirs 可能变化; -files 的 glob 归约为父目录, 全为根级 pattern 时跳过)
	sparseDirs := append(opts.Dirs, gitutil.PatternDirs(opts.Files)...)
	if len(sparseDirs) > 0 {
		setArgs := append([]string{"sparse-checkout", "set"}, sparseDirs...)
		if err := r.RunWithTimeout(ctx, opts.Timeout, workDir, setArgs...); err != nil {
			return err
		}
	}

	// fetch (失败不致命). attempt>=2 时清理残留 shallow.lock (第二次重试起).
	if err := r.RunRetry(ctx, opts.Retries, func(ctx context.Context, attempt int) error {
		if attempt >= 2 {
			cache.CleanShallowLock(workDir)
		}
		return r.RunWithTimeout(ctx, opts.Timeout, workDir, "fetch", "--depth=1", "--no-tags",
			"origin", opts.Ref)
	}, "fetch"); err != nil {
		gitutil.Logf("  fetch 失败, 继续使用缓存旧版本: %v", err)
		gitutil.Logf("Step 1 完成 (%s)", time.Since(t0))
		return nil
	}

	// reset --hard FETCH_HEAD (被 kill 会留 index.lock, 清锁让整体重试能跑 fresh).
	if err := r.RunWithTimeout(ctx, opts.Timeout, workDir, "reset", "--hard", "FETCH_HEAD"); err != nil {
		cache.CleanGitLocks(workDir)
		gitutil.Logf("  reset 失败: %v", err)
		return err
	}

	gitutil.Logf("Step 1 完成 (%s)", time.Since(t0))
	return nil
}
