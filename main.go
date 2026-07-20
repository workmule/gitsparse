package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/workmule/gitsparse/internal/gitutil"
	"github.com/workmule/gitsparse/internal/puller"

	// 注册各拉取模式实现 (init() 中 Register)
	_ "github.com/workmule/gitsparse/internal/puller/fullpull"
	_ "github.com/workmule/gitsparse/internal/puller/snip"
)

// 版本号 — 每次发版修改此值 (格式: vx.x.x)
// 通过 ldflags 可在构建时覆盖: go build -ldflags "-X 'main.Version=v2.1.0'"
const Version = "v2.1.0"

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
	repo := flag.String("repo", "", "Git repository URL")
	ref := flag.String("ref", "", "Git ref: branch name, tag, or commit SHA")
	dirs := flag.String("dirs", "", "Comma-separated directory paths to pull")
	output := flag.String("output", ".", "Output directory")
	timeout := flag.Duration("timeout", time.Minute, "Timeout per network operation (clone/fetch/lfs); 0 = no timeout")
	retries := flag.Int("retries", 3, "Retry count for network operations")
	cacheDir := flag.String("cache-dir", filepath.Join(os.TempDir(), cachePrefix), "Cache directory for cloned repos")
	cacheTTL := flag.Duration("cache-ttl", 24*time.Hour, "Cache TTL; entries older than this are cleaned up (0 = no cleanup)")
	noCache := flag.Bool("no-cache", false, "Skip cache, force fresh clone")
	noLFS := flag.Bool("no-lfs", false, "Skip Git LFS pull (LFS files will be pointers, not real content)")
	mode := flag.String("mode", "full", "Pull mode (available: "+puller.AvailableModes()+")")
	listModes := flag.Bool("list-modes", false, "List available pull modes and exit")
	version := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *version {
		fmt.Printf("gitsparse %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		return
	}

	if *listModes {
		fmt.Printf("Available pull modes: %s\n", puller.AvailableModes())
		return
	}

	if *repo == "" || *ref == "" || *dirs == "" {
		fmt.Fprintln(os.Stderr, "[FAIL] -repo, -ref, -dirs are required")
		flag.Usage()
		os.Exit(1)
	}

	// 打印 git 版本 (便于流水线环境排查); 找不到 git 直接报错退出
	out, err := exec.Command("git", "--version").CombinedOutput()
	if err != nil {
		gitutil.Failf("找不到 git, 请确认已安装并加入 PATH: %v", err)
	}
	fmt.Printf("[git] %s", out)

	dirList := gitutil.SplitAndTrim(*dirs, ",")
	if len(dirList) == 0 {
		gitutil.Failf("没有指定要拉取的目录")
	}

	// git 执行器: 超时 + 重试
	runner := gitutil.Runner{Timeout: *timeout, Retries: *retries}

	gitutil.Logf("配置: mode=%s, timeout=%s, retries=%d, cache=%s, ttl=%s",
		*mode,
		gitutil.DurStr(runner.Timeout), runner.Retries,
		gitutil.BoolStr(*noCache, "off", *cacheDir), gitutil.DurStr(*cacheTTL))

	// 选择拉取模式
	p, err := puller.Get(*mode)
	if err != nil {
		gitutil.Failf("%v", err)
	}

	// 构造 Options
	opts := puller.Options{
		Repo:     *repo,
		Ref:      *ref,
		Dirs:     dirList,
		Output:   *output,
		CacheDir: *cacheDir,
		NoCache:  *noCache,
		NoLFS:    *noLFS,
		CacheTTL: *cacheTTL,
		Runner:   &runner,
	}

	// 执行拉取 (公共流程: 缓存检测→拉取→LFS→拷贝→清理)
	if err := puller.Run(p, opts); err != nil {
		gitutil.Failf("%v", err)
	}
}
