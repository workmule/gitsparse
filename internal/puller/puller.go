// Package puller 定义 git 目录拉取的抽象接口与模式注册表.
//
// 设计目标:
//   - 把"如何拉取 git 仓库子目录"抽象成 Puller 接口
//   - 每种拉取策略 (全量 clone / sparse-checkout / 第三方库封装 / ...) 实现该接口
//   - 通过 Register() 注册, main 按 -mode 参数选择实现
//   - 新增模式只需实现接口 + 在 init() 中 Register, 无需修改 main.go
//
// 用法:
//
//	p, err := puller.Get("full")
//	if err != nil { ... }
//	err = p.Pull(opts)
//
// 新增模式示例:
//
//	package mymode
//	import "github.com/workmule/gitsparse/internal/puller"
//	func init() { puller.Register("mymode", &MyPuller{}) }
//	type MyPuller struct{}
//	func (p *MyPuller) Pull(opts puller.Options) error { ... }
package puller

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/workmule/gitsparse/internal/gitutil"
)

// ============================================================================
// Options
// ============================================================================

// Options 是所有拉取模式共享的输入参数.
// 拉取模式实现可读取所需字段, 忽略无关字段.
type Options struct {
	// Repo 远端仓库 URL (必填).
	Repo string

	// Ref 目标 ref: 分支名 / 标签 / commit SHA (必填).
	Ref string

	// Dirs 要拉取的子目录列表 (必填, 至少 1 个).
	Dirs []string

	// Output 输出目录, 拉取的子目录会拷贝到 Output/<dir>.
	Output string

	// CacheDir 缓存根目录, 各模式可在此下按 CacheKey 创建子目录.
	CacheDir string

	// NoCache true 时跳过缓存, 强制全新拉取.
	NoCache bool

	// NoLFS true 时跳过 Git LFS 拉取.
	NoLFS bool

	// CacheTTL 缓存过期清理时间; 0 表示不清理.
	CacheTTL time.Duration

	// Runner 公共 git 命令执行器 (含 timeout / retries).
	// nil 时各模式应自行 new 一个默认 Runner.
	Runner *gitutil.Runner
}

// CacheKey 返回 repo+ref 的哈希, 用作缓存子目录名.
func (o Options) CacheKey() string {
	return gitutil.CacheHash(o.Repo, o.Ref)
}

// CachePath 返回完整缓存子目录路径 = CacheDir/CacheKey.
func (o Options) CachePath() string {
	return filepath.Join(o.CacheDir, o.CacheKey())
}

// RunnerOrNew 返回 opts.Runner; 为 nil 时返回一个默认 Runner (无超时, 不重试).
func (o Options) RunnerOrNew() *gitutil.Runner {
	if o.Runner != nil {
		return o.Runner
	}
	return &gitutil.Runner{}
}

// ============================================================================
// Puller 接口
// ============================================================================

// Puller 抽象一种"把远端仓库弄到本地工作区"的拉取策略.
//
// 核心解耦: 模式只负责 git 拉取 (clone/fetch/reset),
// 缓存命中检测 / LFS / 拷贝到输出 / 缓存清理 等通用流程由 puller.Run 统一处理.
//
// 实现要点:
//   - FetchRepo 应让 workDir 成为指向 opts.Repo@opts.Ref 的有效 git 工作区
//   - cached=true 表示 workDir 已是上次的缓存 (含 .git), 走 fetch+reset 增量更新
//   - cached=false 表示 workDir 不存在或无效, 走全新 clone
//   - 失败时返回 error, 由调用方 (puller.Run) 决定后续处理
type Puller interface {
	// Name 返回模式名 (注册时的 key), 如 "full", "sparse".
	Name() string

	// Desc 返回模式简短描述, 用于 -mode 帮助.
	Desc() string

	// FetchRepo 把仓库拉到 workDir.
	// cached 为 true 时 workDir 已有缓存, 应增量更新 (fetch+reset);
	// cached 为 false 时应全新 clone.
	FetchRepo(opts Options, workDir string, cached bool) error
}

// Run 执行完整的拉取流程 (缓存检测 → 拉取 → LFS → 拷贝 → 清理过期缓存).
// 这是所有模式共享的公共编排, 各模式只需实现 FetchRepo.
func Run(p Puller, opts Options) error {
	r := opts.RunnerOrNew()
	workDir := opts.CachePath()
	cache := Cache{NoCache: opts.NoCache}
	start := time.Now()

	// Step 1: 缓存命中检测 + 拉取 (模式实现)
	cached := cache.Hit(workDir)
	if err := p.FetchRepo(opts, workDir, cached); err != nil {
		return err
	}

	// Step 2: LFS pull (通用)
	if err := runLFS(r, opts, workDir); err != nil {
		return err
	}

	// Step 3: 拷贝目录到输出 (通用)
	if err := CopyDirsToOutput(workDir, opts.Output, opts.Dirs); err != nil {
		return err
	}

	gitutil.Logf("全部完成, 总耗时 %s", time.Since(start))

	// Step 4: 清理过期缓存 (通用)
	cache.CleanExpired(opts.CacheDir, opts.CacheTTL)
	return nil
}

// runLFS 通用 LFS 拉取: 若仓库含 LFS 文件且未禁用, 执行 git lfs pull --include <dirs>.
func runLFS(r *gitutil.Runner, opts Options, workDir string) error {
	if opts.NoLFS {
		gitutil.Logf("Step 2: 跳过 LFS pull (-no-lfs)")
		return nil
	}
	if !gitutil.HasLFSFiles(workDir) {
		return nil
	}
	lfsIncludes := gitutil.LFSIncludePatterns(opts.Dirs)
	lfsIncludeArg := strings.Join(lfsIncludes, ",")
	gitutil.Logf("Step 2: git lfs pull --include=%s", lfsIncludeArg)
	t0 := time.Now()
	r.Run("", "lfs", "install")
	if err := r.RunRetry(func() error {
		return r.Run(workDir, "lfs", "pull", "--include", lfsIncludeArg)
	}, "lfs pull"); err != nil {
		return err
	}
	gitutil.Logf("Step 2 完成 (%s)", time.Since(t0))
	return nil
}

// ============================================================================
// Registry
// ============================================================================

var (
	registryMu sync.RWMutex
	registry   = map[string]Puller{}
)

// Register 注册一个拉取模式. 重复注册同名模式会 panic (启动期错误, 应尽早暴露).
// 通常在包 init() 中调用.
func Register(p Puller) {
	registryMu.Lock()
	defer registryMu.Unlock()
	name := p.Name()
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("puller: duplicate registration for mode %q", name))
	}
	registry[name] = p
}

// Get 按名字获取已注册的拉取模式. 不存在时返回错误.
func Get(name string) (Puller, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("puller: unknown mode %q (available: %s)", name, AvailableModes())
	}
	return p, nil
}

// AvailableModes 返回所有已注册模式名, 按字母序排序, 逗号分隔.
func AvailableModes() string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return joinStrings(names, ", ")
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}
