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
	"context"
	"fmt"
	"os"
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

	// Dirs 要拉取的子目录列表 (-dirs), 整目录拷贝到输出.
	Dirs []string

	// Files 要拉取的文件路径或 glob 模式列表 (-files), 如 "common/protocol/*.xml".
	// 拉取层归约为父目录, 拷贝层按 glob 匹配. 与 Dirs 至少给一个.
	Files []string

	// Output 输出目录, 拉取的子目录会拷贝到 Output/<dir>.
	Output string

	// CacheDir 缓存根目录, 各模式可在此下按 CacheKey 创建子目录.
	CacheDir string

	// Mode 拉取模式名 ("full" / "snip" ...), 参与 CacheKey 以隔离不兼容的工作区.
	Mode string
	// NoCache true 时跳过缓存, 强制全新拉取.
	NoCache bool

	// NoLFS true 时跳过 Git LFS 拉取.
	NoLFS bool

	// SkipMissingDirs true 时, -dirs 中在仓库里不存在的目录跳过而非报错.
	SkipMissingDirs bool

	// SkipMissingFiles true 时, -files 中无匹配文件的 pattern 跳过而非报错.
	SkipMissingFiles bool

	// CacheTTL 缓存过期清理时间; 0 表示不清理.
	CacheTTL time.Duration

	// Timeout 单个 git 子命令的执行超时; 0 = 不限.
	// 由 Run 时用 context.WithTimeout 派生子 ctx 控制.
	Timeout time.Duration

	// Retries 子步骤重试次数 (单条 git 命令失败后的重试, 由 Runner.RunRetry 处理).
	Retries int

	// FetchRetries 整体重试次数: FetchRepo 整体失败后清理 workDir 重跑的次数.
	// 0=不整体重试; >0=失败后清理重跑.
	// 与 Retries (子步骤重试) 相互独立:
	// 子步骤重试覆盖瞬时网络抖动, 整体重试覆盖上下文损坏 (如 .git 残缺).
	FetchRetries int

	// TotalTimeout 整个 gitsparse 运行的总超时; 0 = 不限.
	// 由 Run 时创建根 context.WithTimeout 控制, 所有子步骤共享.
	TotalTimeout time.Duration

	// Version / ListModes 是 CLI 早退标志 (非 Puller 参数), 由 main 在调用 Run 前检查.
	// 放在 Options 里只为让 parseFlags 单一返回值覆盖所有 flag.
	Version   bool
	ListModes bool
}

// Validate 检查必填字段. 返回 nil 表示通过.
// 早退标志 (Version/ListModes) 不在此检查: 命中时 main 应直接退出, 不走校验.
func (o Options) Validate() error {
	if o.Repo == "" {
		return fmt.Errorf("-repo is required")
	}
	if o.Ref == "" {
		return fmt.Errorf("-ref is required")
	}
	if len(o.Dirs) == 0 && len(o.Files) == 0 {
		return fmt.Errorf("至少指定一个要拉取的目录 (-dirs) 或文件/glob 模式 (-files)")
	}
	return nil
}

// CacheKey 返回 repo+ref+mode 的哈希, 用作缓存子目录名.
// mode 参与 hash: full (全量检出) 与 snip (sparse-checkout) 的工作区互不兼容,
// 共用同一缓存目录会导致 sparse 配置残留 / 文件缺失, 必须隔离.
func (o Options) CacheKey() string {
	return gitutil.CacheHash(o.Repo, o.Ref, o.Mode)
}

// CachePath 返回完整缓存子目录路径 = CacheDir/CacheKey.
func (o Options) CachePath() string {
	return filepath.Join(o.CacheDir, o.CacheKey())
}

// RunnerOrNew 返回一个无状态 Runner. 保留兼容旧调用方; Runner 现已无状态.
func (o Options) RunnerOrNew() *gitutil.Runner {
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
	// ctx 控制超时/取消; 调用方 (puller.Run) 已派生好子 ctx.
	// cached 为 true 时 workDir 已有缓存, 应增量更新 (fetch+reset);
	// cached 为 false 时应全新 clone.
	FetchRepo(ctx context.Context, opts Options, workDir string, cached bool) error
}

// Run 执行完整的拉取流程 (缓存检测 → 拉取 → LFS → 拷贝 → 清理过期缓存).
// 这是所有模式共享的公共编排, 各模式只需实现 FetchRepo.
// ctx 由调用方 (main) 传入; TotalTimeout > 0 时 Run 内部派生根 ctx 控制总超时.
func Run(p Puller, opts Options) error {
	ctx := context.Background()
	var cancel context.CancelFunc
	if opts.TotalTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, opts.TotalTimeout)
		defer cancel()
	}

	r := opts.RunnerOrNew()
	// 用 Puller 的实际模式名参与缓存 key, 隔离 full/snip 等不兼容工作区.
	// 覆盖 opts.Mode: 调用方可能未填, 以注册的实现为准.
	opts.Mode = p.Name()
	workDir := opts.CachePath()
	cache := Cache{NoCache: opts.NoCache}
	start := time.Now()

	// Step 1: 缓存检测 + FetchRepo (含整体重试)
	if err := fetchWithRetry(ctx, p, opts, workDir, cache); err != nil {
		return err
	}

	// Step 2: LFS pull (通用)
	if err := runLFS(ctx, r, opts, workDir); err != nil {
		return err
	}

	// Step 3: 拷贝目录/文件到输出 (通用)
	if err := CopyDirsToOutput(workDir, opts.Output, opts.Dirs, opts.SkipMissingDirs); err != nil {
		return err
	}
	if err := CopyFilesToOutput(workDir, opts.Output, opts.Files, opts.Dirs, opts.SkipMissingFiles); err != nil {
		return err
	}

	gitutil.Logf("全部完成, 总耗时 %s", time.Since(start))

	// Step 4: 清理过期缓存 (通用)
	cache.CleanExpired(opts.CacheDir, opts.CacheTTL)
	return nil
}

// fetchWithRetry 执行 FetchRepo, 支持整体重试.
// 复用 Runner.RunRetry 作通用重试框架, 闭包内决定清理动作:
//   - attempt=0: 首次尝试, 按 cache.Hit 结果走 fresh 或 cache 路径
//   - attempt>0: 整体重试, 先 os.RemoveAll(workDir) 清理残留, 再走 fresh (Hit 必返回 false)
//
// 子步骤重试 (RunRetry) 由各 FetchRepo 内部处理, 覆盖瞬时网络抖动;
// 整体重试在 FetchRepo 整体失败后清理 workDir 再重跑, 覆盖上下文损坏 (如 .git 残缺).
func fetchWithRetry(ctx context.Context, p Puller, opts Options, workDir string, cache Cache) error {
	r := opts.RunnerOrNew()
	return r.RunRetry(ctx, opts.FetchRetries, func(ctx context.Context, attempt int) error {
		if attempt > 0 {
			gitutil.Logf("Step 1: 整体重试 %d/%d (清理 workDir 后重跑)", attempt, opts.FetchRetries)
			os.RemoveAll(workDir)
		}
		cached := cache.Hit(workDir)
		return p.FetchRepo(ctx, opts, workDir, cached)
	}, "fetch")
}

// runLFS 通用 LFS 拉取: 若仓库含 LFS 文件且未禁用, 执行 git lfs pull --include <dirs>.
func runLFS(ctx context.Context, r *gitutil.Runner, opts Options, workDir string) error {
	if opts.NoLFS {
		gitutil.Logf("Step 2: 跳过 LFS pull (-no-lfs)")
		return nil
	}
	if !gitutil.HasLFSFiles(workDir) {
		return nil
	}
	lfsIncludes := gitutil.LFSIncludePatterns(opts.Dirs, opts.Files)
	lfsIncludeArg := strings.Join(lfsIncludes, ",")
	gitutil.Logf("Step 2: git lfs pull --include=%s", lfsIncludeArg)
	t0 := time.Now()
	r.RunWithTimeout(ctx, opts.Timeout, "", "lfs", "install")
	if err := r.RunRetry(ctx, opts.Retries, func(ctx context.Context, attempt int) error {
		return r.RunWithTimeout(ctx, opts.Timeout, workDir, "lfs", "pull", "--include", lfsIncludeArg)
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
