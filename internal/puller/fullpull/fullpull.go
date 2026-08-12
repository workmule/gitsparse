// Package fullpull 实现"全量拉取"模式: git init + fetch --depth=1 全量检出.
//
// 这是 gitsparse v2.0 的默认模式. 设计说明见 main.go 顶部注释.
//
// v2.2 重构: 把 git clone 拆成 init + remote + fetch + checkout.
// 原因: git clone 失败留非空目录, 重试必报 "destination exists",
// 无法子步骤重试; 拆分后 fetch 失败只留 shallow.lock, 清掉即可重试.
//
// 本包只负责"把仓库弄到 workDir" (FetchRepo 实现):
//   - cached=false (全新): init + remote add + fetch --depth=1 + checkout
//   - cached=true (复用): git fetch --depth=1 origin <ref> + git reset --hard <ref>
//
// LFS 拉取 / 拷贝到输出 / 缓存命中检测 / 过期缓存清理 等通用流程
// 由 internal/puller.Run 统一编排, 本包不涉及.
package fullpull

import (
	"context"
	"os"
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

// Puller 全量拉取模式实现.
type Puller struct{}

// Name 模式名.
func (p *Puller) Name() string { return "full" }

// Desc 模式简短描述.
func (p *Puller) Desc() string {
	return "全量拉取: git init + fetch --depth=1 全量检出后拷贝指定目录 (兼容性最好)"
}

// FetchRepo 把仓库拉到 workDir.
// cached=true 走增量更新 (fetch+reset); false 走全新拉取.
func (p *Puller) FetchRepo(ctx context.Context, opts puller.Options, workDir string, cached bool) error {
	r := opts.RunnerOrNew()
	isSHA := gitutil.IsCommitSHA(opts.Ref)

	if !cached {
		return p.freshFetch(ctx, r, opts, workDir, isSHA)
	}
	return p.cacheUpdate(ctx, r, opts, workDir, isSHA)
}

// freshFetch 全新拉取: init + remote + fetch --depth=1 + checkout.
// 拆分 git clone 是为了让 fetch 失败可子步骤重试 (清 shallow.lock 即可),
// 而不是像 clone 失败那样因 "destination exists" 无法重试, 只能整体重跑.
func (p *Puller) freshFetch(ctx context.Context, r *gitutil.Runner, opts puller.Options, workDir string, isSHA bool) error {
	gitutil.Logf("Step 1: git init + fetch --depth=1")
	t0 := time.Now()

	gitutil.PrepareCloneTarget(workDir)
	// git init 需要目标目录已存在 (git clone 会自动创建, init 不会)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return err
	}

	// 1. init + remote (轻量操作, 不重试)
	if err := r.RunWithTimeout(ctx, opts.Timeout, workDir, "init"); err != nil {
		return err
	}
	if err := r.RunWithTimeout(ctx, opts.Timeout, workDir, "remote", "add", "origin", opts.Repo); err != nil {
		return err
	}

	// 2. fetch --depth=1 (重网络操作, 子步骤重试)
	// attempt>=2 时清理残留 shallow.lock (fetch 中断会留下, 导致重试撞锁).
	if err := r.RunRetry(ctx, opts.Retries, func(ctx context.Context, attempt int) error {
		if attempt >= 2 {
			puller.Cache{}.CleanShallowLock(workDir)
		}
		return r.RunWithTimeout(ctx, opts.Timeout, workDir, "fetch", "--depth=1", "--no-tags",
			"origin", opts.Ref)
	}, "fetch"); err != nil {
		return err
	}

	// 3. checkout FETCH_HEAD (分支/标签) 或指定 SHA
	// fetch 后 FETCH_HEAD 指向刚拉的 ref; checkout 把工作区切到该 commit.
	// checkout 被 kill 会留 index.lock, 清锁让整体重试能跑 fresh.
	checkoutTarget := "FETCH_HEAD"
	if isSHA {
		checkoutTarget = opts.Ref
	}
	if err := r.RunWithTimeout(ctx, opts.Timeout, workDir, "checkout", checkoutTarget); err != nil {
		puller.Cache{}.CleanGitLocks(workDir)
		return err
	}

	gitutil.Logf("Step 1 完成 (%s)", time.Since(t0))
	return nil
}

// cacheUpdate 缓存复用流程: 清理 shallow.lock → fetch → reset --hard <ref>.
// fetch 失败不致命 (离线运行继续用旧缓存); reset 失败清除缓存并返回错误.
func (p *Puller) cacheUpdate(ctx context.Context, r *gitutil.Runner, opts puller.Options, workDir string, isSHA bool) error {
	gitutil.Logf("Step 1: 缓存复用, 拉取最新更新")
	t0 := time.Now()

	// 外层清理上次运行残留的 shallow.lock; 闭包内 attempt>=2 清理本次重试间残留.
	cache := puller.Cache{NoCache: opts.NoCache}
	cache.CleanShallowLock(workDir)

	if err := r.RunRetry(ctx, opts.Retries, func(ctx context.Context, attempt int) error {
		if attempt >= 2 {
			cache.CleanShallowLock(workDir)
		}
		return r.RunWithTimeout(ctx, opts.Timeout, workDir, "fetch", "--depth=1", "--no-tags", "origin", opts.Ref)
	}, "fetch"); err != nil {
		gitutil.Logf("  fetch 失败, 继续使用缓存旧版本: %v", err)
		gitutil.Logf("Step 1 完成 (%s)", time.Since(t0))
		return nil
	}

	resetTarget := opts.Ref
	if !isSHA {
		resetTarget = "origin/" + opts.Ref
	}
	gitutil.Logf("  git reset --hard %s", resetTarget)
	if err := r.RunWithTimeout(ctx, opts.Timeout, workDir, "reset", "--hard", resetTarget); err != nil {
		// reset 被 kill 会留 index.lock, 清锁让整体重试能跑 fresh (不删缓存, 保留 fetch 成果).
		cache.CleanGitLocks(workDir)
		gitutil.Logf("  reset 失败: %v", err)
		return err
	}
	gitutil.Logf("Step 1 完成 (%s)", time.Since(t0))
	return nil
}
