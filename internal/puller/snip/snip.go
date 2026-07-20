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
func (p *Puller) FetchRepo(opts puller.Options, workDir string, cached bool) error {
	// Git 版本检测: snip 模式需要 sparse-checkout --cone (Git 2.25+)
	if err := p.checkGitVersion(); err != nil {
		return err
	}
	if !cached {
		return p.freshSparseFetch(opts, workDir)
	}
	return p.cacheSparseUpdate(opts, workDir)
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
func (p *Puller) freshSparseFetch(opts puller.Options, workDir string) error {
	r := opts.RunnerOrNew()
	gitutil.Logf("Step 1: git init + sparse-checkout (cone)")
	t0 := time.Now()

	gitutil.PrepareCloneTarget(workDir)
	// git init 需要目标目录已存在 (full 模式的 git clone 会自动创建, init 不会)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return err
	}

	// 1. init + remote
	if err := r.Run(workDir, "init"); err != nil {
		return err
	}
	if err := r.Run(workDir, "remote", "add", "origin", opts.Repo); err != nil {
		return err
	}

	// 2. sparse-checkout init --cone + set <dirs>
	if err := r.Run(workDir, "sparse-checkout", "init", "--cone"); err != nil {
		return err
	}
	setArgs := append([]string{"sparse-checkout", "set"}, opts.Dirs...)
	if err := r.Run(workDir, setArgs...); err != nil {
		return err
	}

	// 3. fetch --depth=1
	if err := r.RunRetry(func() error {
		return r.Run(workDir, "fetch", "--depth=1", "--no-tags",
			"--progress", "origin", opts.Ref)
	}, "fetch"); err != nil {
		return err
	}

	// 4. checkout FETCH_HEAD
	if err := r.Run(workDir, "checkout", "FETCH_HEAD"); err != nil {
		return err
	}

	gitutil.Logf("Step 1 完成 (%s)", time.Since(t0))
	return nil
}

// cacheSparseUpdate 缓存复用: reapply sparse + fetch + reset.
func (p *Puller) cacheSparseUpdate(opts puller.Options, workDir string) error {
	r := opts.RunnerOrNew()
	gitutil.Logf("Step 1: 缓存复用, sparse-checkout 更新")
	t0 := time.Now()

	cache := puller.Cache{NoCache: opts.NoCache}
	cache.CleanShallowLock(workDir)

	// sparse-checkout set (dirs 可能变化)
	setArgs := append([]string{"sparse-checkout", "set"}, opts.Dirs...)
	if err := r.Run(workDir, setArgs...); err != nil {
		return err
	}

	// fetch (失败不致命)
	if err := r.RunRetry(func() error {
		return r.Run(workDir, "fetch", "--depth=1", "--no-tags",
			"--progress", "origin", opts.Ref)
	}, "fetch"); err != nil {
		gitutil.Logf("  fetch 失败, 继续使用缓存旧版本: %v", err)
		gitutil.Logf("Step 1 完成 (%s)", time.Since(t0))
		return nil
	}

	// reset --hard FETCH_HEAD
	if err := r.Run(workDir, "reset", "--hard", "FETCH_HEAD"); err != nil {
		gitutil.Logf("  reset 失败, 清除缓存目录: %s", workDir)
		os.RemoveAll(workDir)
		return err
	}

	gitutil.Logf("Step 1 完成 (%s)", time.Since(t0))
	return nil
}
