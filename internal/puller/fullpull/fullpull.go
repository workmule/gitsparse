// Package fullpull 实现"全量拉取"模式: git clone --depth=1 全量检出.
//
// 这是 gitsparse v2.0 的默认模式. 设计说明见 main.go 顶部注释.
//
// 本包只负责"把仓库弄到 workDir" (FetchRepo 实现):
//   - cached=false (全新): git clone --depth=1 [--branch <ref>] <repo> <workDir>
//     commit SHA 需 clone 默认分支后 fetch + checkout
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
	return "全量拉取: git clone --depth=1 全量检出后拷贝指定目录 (兼容性最好)"
}

// FetchRepo 把仓库拉到 workDir.
// cached=true 走增量更新 (fetch+reset); false 走全新 clone.
func (p *Puller) FetchRepo(ctx context.Context, opts puller.Options, workDir string, cached bool) error {
	r := opts.RunnerOrNew()
	isSHA := gitutil.IsCommitSHA(opts.Ref)

	if !cached {
		return p.freshClone(ctx, r, opts, workDir, isSHA)
	}
	return p.cacheUpdate(ctx, r, opts, workDir, isSHA)
}

// freshClone 全新 clone 流程: 分支/标签直接 --branch; commit SHA 需 clone 默认分支后 fetch + checkout.
func (p *Puller) freshClone(ctx context.Context, r *gitutil.Runner, opts puller.Options, workDir string, isSHA bool) error {
	gitutil.PrepareCloneTarget(workDir)

	if isSHA {
		return p.cloneSHA(ctx, r, opts, workDir)
	}
	return p.cloneBranchOrTag(ctx, r, opts, workDir)
}

// cloneSHA commit SHA 流程: clone 默认分支 → fetch SHA → checkout SHA.
func (p *Puller) cloneSHA(ctx context.Context, r *gitutil.Runner, opts puller.Options, workDir string) error {
	gitutil.Logf("Step 1: git clone --depth=1 (默认分支, 随后 fetch SHA)")
	t0 := time.Now()
	if err := r.RunRetry(ctx, opts.Retries, func(ctx context.Context, attempt int) error {
		gitutil.PrepareCloneTarget(workDir)
		return r.RunWithTimeout(ctx, opts.Timeout, "", "clone", "--depth=1", "--no-tags", "--progress", opts.Repo, workDir)
	}, "clone"); err != nil {
		return err
	}
	gitutil.Logf("Step 1a 完成 (%s)", time.Since(t0))

	gitutil.Logf("Step 1b: git fetch --depth=1 origin %s", opts.Ref)
	t0b := time.Now()
	// attempt 0=首次, 1=第一次重试(直接重试), >=2=第二次起清理残留 shallow.lock.
	if err := r.RunRetry(ctx, opts.Retries, func(ctx context.Context, attempt int) error {
		if attempt >= 2 {
			puller.Cache{}.CleanShallowLock(workDir)
		}
		return r.RunWithTimeout(ctx, opts.Timeout, workDir, "fetch", "--depth=1", "--no-tags", "--progress", "origin", opts.Ref)
	}, "fetch"); err != nil {
		return err
	}
	if err := r.RunWithTimeout(ctx, opts.Timeout, workDir, "checkout", "--progress", opts.Ref); err != nil {
		return err
	}
	gitutil.Logf("Step 1b 完成 (%s)", time.Since(t0b))
	return nil
}

// cloneBranchOrTag 分支/标签流程: clone --branch <ref>.
func (p *Puller) cloneBranchOrTag(ctx context.Context, r *gitutil.Runner, opts puller.Options, workDir string) error {
	gitutil.Logf("Step 1: git clone --depth=1 --branch %s", opts.Ref)
	t0 := time.Now()
	if err := r.RunRetry(ctx, opts.Retries, func(ctx context.Context, attempt int) error {
		gitutil.PrepareCloneTarget(workDir)
		return r.RunWithTimeout(ctx, opts.Timeout, "", "clone", "--depth=1", "--no-tags",
			"--branch", opts.Ref, "--progress", opts.Repo, workDir)
	}, "clone"); err != nil {
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
		return r.RunWithTimeout(ctx, opts.Timeout, workDir, "fetch", "--depth=1", "--no-tags", "--progress", "origin", opts.Ref)
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
		gitutil.Logf("  reset 失败, 清除缓存目录: %s", workDir)
		os.RemoveAll(workDir)
		return err
	}
	gitutil.Logf("Step 1 完成 (%s)", time.Since(t0))
	return nil
}
