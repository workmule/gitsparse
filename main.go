package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/workmule/gitsparse/internal/gitutil"
	"github.com/workmule/gitsparse/internal/puller"

	// 注册各拉取模式实现 (init() 中 Register)
	_ "github.com/workmule/gitsparse/internal/puller/fullpull"
	_ "github.com/workmule/gitsparse/internal/puller/snip"
)

// 版本号 — 每次发版修改此值 (格式: vx.x.x)
// 通过 ldflags 可在构建时覆盖: go build -ldflags "-X 'main.Version=v2.2.0'"
const Version = "v2.2.20260826123858"

// ============================================================================
// 设计说明 (v2.1.0 重构: 模式化)
// ============================================================================
// v2.0 把拉取逻辑从 sparse-checkout 改成全量 clone, 兼容性最好.
// 但后续长期迭代可能需要多种拉取方式并存 (sparse / 第三方库 / 增量同步 ...),
// 因此 v2.1 把拉取逻辑抽象成 puller.Puller 接口:
//   - main 只负责 CLI 解析 + 选择模式 + 调用 Pull()
//   - 每种模式实现自己的 Pull(), 在 init() 中 puller.Register()
//   - 新增模式无需修改 main, 只需新包 + 空白导入
//
// 包结构:
//   - internal/gitutil: 公共工具 (log/runGit/copyDir/cacheHash/...)
//   - internal/puller:  接口定义 + 注册表
//   - internal/puller/fullpull: 全量拉取模式 (v2.0 逻辑迁移)
// ============================================================================

// 缓存目录前缀
const cachePrefix = "gitsparse-cache"

func main() {
	opts := parseFlags()

	// 打印版本号
	if opts.Version {
		fmt.Printf("gitsparse %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		return
	}
	// 打印模式列表
	if opts.ListModes {
		fmt.Printf("Available pull modes: auto, %s\n", puller.AvailableModes())
		fmt.Println("  auto: 按本地 git 版本自动选择, >= 2.25 用 snip, 否则用 full")
		return
	}
	// 检查参数合法性
	if err := opts.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "[FAIL]", err)
		flag.Usage()
		os.Exit(1)
	}

	// 打印 git 版本 (便于流水线环境排查); 找不到 git 直接报错退出
	out, err := exec.Command("git", "--version").CombinedOutput()
	if err != nil {
		gitutil.Failf("找不到 git, 请确认已安装并加入 PATH: %v", err)
	}
	fmt.Printf("[git] %s", out)

	// auto 模式: 按本地 Git 版本解析为实际模式 (snip/full), 见 docs/design/prd-auto-mode.md.
	// 必须在 puller.Get 之前: auto 是 CLI 层虚拟值, 未注册到模式注册表.
	if opts.Mode == "auto" {
		resolved, reason := resolveAutoMode(string(out))
		gitutil.Logf("auto: %s", reason)
		opts.Mode = resolved
	}

	gitutil.Logf("配置: mode=%s, timeout=%s, retries=%d, fetch-retries=%d, total-timeout=%s, cache=%s, ttl=%s",
		opts.Mode,
		gitutil.DurStr(opts.Timeout), opts.Retries, opts.FetchRetries, gitutil.DurStr(opts.TotalTimeout),
		gitutil.BoolStr(opts.NoCache, "off", opts.CacheDir), gitutil.DurStr(opts.CacheTTL))

	// 选择拉取模式
	p, err := puller.Get(opts.Mode)
	if err != nil {
		gitutil.Failf("%v", err)
	}

	// 执行拉取 (公共流程: 缓存检测→拉取→LFS→拷贝→清理)
	if err := puller.Run(p, opts); err != nil {
		gitutil.Failf("%v", err)
	}
}

// resolveAutoMode 根据 `git --version` 的输出决定 auto 模式实际使用的模式名.
// snip 需要 Git 2.25+ (sparse-checkout --cone); 版本不足或解析失败时保守退回 full.
// 返回 (模式名, 决策原因); 原因用于日志, 帮助排查"为什么这台机器跑的是 full".
func resolveAutoMode(gitVersionOutput string) (string, string) {
	ver, err := gitutil.ParseGitVersion(gitVersionOutput)
	if err != nil {
		return "full", fmt.Sprintf("无法解析 git 版本输出 %q, 保守使用 full",
			strings.TrimSpace(gitVersionOutput))
	}
	if gitutil.SupportsSparseCheckoutCone(ver) {
		return "snip", fmt.Sprintf("git %d.%d.%d >= 2.25, 支持 sparse-checkout", ver[0], ver[1], ver[2])
	}
	return "full", fmt.Sprintf("git %d.%d.%d < 2.25, 不支持 sparse-checkout, 降级 full", ver[0], ver[1], ver[2])
}

// parseFlags 解析 CLI flag 并整理成 puller.Options.
// -dirs (逗号分隔字符串) 在此拆成 []string 后填入 opts.Dirs.
// 早退标志 (Version/ListModes) 也写入 opts, 由 main 检查.
func parseFlags() puller.Options {
	repo := flag.String("repo", "", "Git repository URL")
	ref := flag.String("ref", "", "Git ref: branch name, tag, or commit SHA")
	dirs := flag.String("dirs", "", "Comma-separated directory paths to pull")
	output := flag.String("output", ".", "Output directory")
	timeout := flag.Duration("timeout", time.Minute, "Timeout per network operation (clone/fetch/lfs); 0 = no timeout")
	retries := flag.Int("retries", 3, "Retry count for network operations")
	fetchRetries := flag.Int("fetch-retries", 1, "FetchRepo overall retry count (clean workDir and re-run on failure; 0 = no overall retry)")
	totalTimeout := flag.Duration("total-timeout", 0, "Total timeout for entire gitsparse run (0 = no limit)")
	cacheDir := flag.String("cache-dir", filepath.Join(os.TempDir(), cachePrefix), "Cache directory for cloned repos")
	cacheTTL := flag.Duration("cache-ttl", 24*time.Hour, "Cache TTL; entries older than this are cleaned up (0 = no cleanup)")
	noCache := flag.Bool("no-cache", false, "Skip cache, force fresh clone")
	noLFS := flag.Bool("no-lfs", false, "Skip Git LFS pull (LFS files will be pointers, not real content)")
	skipMissingDirs := flag.Bool("skip-missing-dirs", false, "Skip dirs that don't exist in the repo instead of failing")
	mode := flag.String("mode", "full", "Pull mode (auto = detect git version; available: auto, "+puller.AvailableModes()+")")
	listModes := flag.Bool("list-modes", false, "List available pull modes and exit")
	version := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	dirList := gitutil.SplitAndTrim(*dirs, ",")

	return puller.Options{
		Repo:            *repo,
		Ref:             *ref,
		Dirs:            dirList,
		Output:          *output,
		CacheDir:        *cacheDir,
		Mode:            *mode,
		NoCache:         *noCache,
		NoLFS:           *noLFS,
		SkipMissingDirs: *skipMissingDirs,
		CacheTTL:        *cacheTTL,
		Timeout:         *timeout,
		Retries:         *retries,
		FetchRetries:    *fetchRetries,
		TotalTimeout:    *totalTimeout,
		Version:         *version,
		ListModes:       *listModes,
	}
}
