package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// 版本号 — 每次发版修改此值 (格式: vx.x.x)
// 通过 ldflags 可在构建时覆盖: go build -ldflags "-X 'main.Version=v2.0.0'"
const Version = "v2.0.0"

// ============================================================================
// 设计说明 (v2.0.0 重构)
// ============================================================================
// 旧版 (v1.x) 用 sparse-checkout 机制 (--no-checkout + core.sparseCheckout +
// 手写 .git/info/sparse-checkout + read-tree), 在不同 Git 版本 / cone 模式 /
// 缓存复用场景下反复出问题 (文件丢失、skip-worktree 残留、no-op checkout 等).
//
// 新版 (v2.0) 改用最简单直接的 git 用法:
//   1. 缓存目录就是一个完整的 git 仓库 (含工作区, 全量检出)
//   2. 首次: git clone --depth=1 <repo> <cachedir> (直接检出)
//      - 分支/标签: 加 --branch <ref>
//      - commit SHA: clone 默认分支后 fetch 该 SHA
//   3. 缓存复用: git fetch --depth=1 origin <ref> + git reset --hard <ref>
//   4. 从缓存目录拷贝指定子目录到 -output 位置
//
// 不再依赖 sparse-checkout / cone 模式 / read-tree, 兼容性最好, 逻辑最简单.
// 代价: 缓存目录会检出全部文件 (而非只检出指定目录), 但 --depth=1 浅克隆
//       本身只下载最新提交的对象, 工作区文件是本地 checkout, 不增加网络流量.
// ============================================================================

// 全局设置
var globalTimeout time.Duration
var globalRetries int

// 缓存目录前缀
const cachePrefix = "gitsparse-cache"

func main() {
	repo := flag.String("repo", "", "Git repository URL")
	ref := flag.String("ref", "", "Git ref: branch name, tag, or commit SHA")
	dirs := flag.String("dirs", "", "Comma-separated directory paths to pull")
	output := flag.String("output", ".", "Output directory")
	timeout := flag.String("timeout", "1m", "Timeout per network operation (clone/fetch/lfs); 0 = no timeout")
	retries := flag.Int("retries", 3, "Retry count for network operations")
	cacheDir := flag.String("cache-dir", filepath.Join(os.TempDir(), cachePrefix), "Cache directory for cloned repos")
	cacheTTL := flag.String("cache-ttl", "24h", "Cache TTL; entries older than this are cleaned up (0 = no cleanup)")
	noCache := flag.Bool("no-cache", false, "Skip cache, force fresh clone")
	noLFS := flag.Bool("no-lfs", false, "Skip Git LFS pull (LFS files will be pointers, not real content)")
	version := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *version {
		fmt.Printf("gitsparse %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
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
		fail("找不到 git, 请确认已安装并加入 PATH: %v", err)
	}
	fmt.Printf("[git] %s", out) // 自带换行

	dirList := splitAndTrim(*dirs, ",")
	if len(dirList) == 0 {
		fail("没有指定要拉取的目录")
	}

	// 解析超时
	if *timeout != "" && *timeout != "0" {
		d, err := time.ParseDuration(*timeout)
		if err != nil {
			fail("无效的 timeout: %v", err)
		}
		globalTimeout = d
	}
	globalRetries = *retries

	// 解析 cache TTL
	var ttl time.Duration
	if *cacheTTL != "" && *cacheTTL != "0" {
		d, err := time.ParseDuration(*cacheTTL)
		if err != nil {
			fail("无效的 cache-ttl: %v", err)
		}
		ttl = d
	}

	log("配置: timeout=%s, retries=%d, cache=%s, ttl=%s",
		durStr(globalTimeout), globalRetries, boolStr(*noCache, "off", *cacheDir), durStr(ttl))

	// 生成缓存 key: repo + ref 的哈希 (dirs 不影响 clone, 只影响拷贝哪些目录)
	cacheKey := cacheHash(*repo, *ref)
	tmpDir := filepath.Join(*cacheDir, cacheKey)

	// 检查缓存是否可用: 判断 .git 目录是否存在
	cached := false
	if *noCache {
		log("缓存已禁用 (-no-cache), 强制重新克隆")
	} else {
		if info, err := os.Stat(filepath.Join(tmpDir, ".git")); err == nil && info.IsDir() {
			cached = true
			log("缓存命中: %s", tmpDir)
		} else {
			log("缓存未命中, 克隆到: %s", tmpDir)
		}
	}

	// 检测 ref 类型: commit SHA 还是 branch/tag
	isSHA := isCommitSHA(*ref)
	var t0 time.Time

	// Step 1: 准备缓存目录的 git 仓库 (clone 或 fetch+reset)
	if !cached {
		// ---- 非缓存: 全新 clone ----
		// 删除已有目录 (可能是 -no-cache 强制刷新, 或上次 clone 残留)
		if _, err := os.Stat(tmpDir); err == nil {
			log("  清理旧目录: %s", tmpDir)
			os.RemoveAll(tmpDir)
		}
		os.MkdirAll(filepath.Dir(tmpDir), 0755)

		if isSHA {
			// commit SHA: clone 默认分支, 再 fetch 指定 SHA
			log("Step 1: git clone --depth=1 (默认分支, 随后 fetch SHA)")
			t0 = time.Now()
			if err := runGitRetry(func() error {
				return runGit("", "clone",
					"--depth=1", "--no-tags",
					"--progress",
					*repo, tmpDir,
				)
			}, "clone"); err != nil {
				fail("git clone 失败: %v", err)
			}
			log("Step 1a 完成 (%s)", time.Since(t0))

			log("Step 1b: git fetch --depth=1 origin %s", *ref)
			t0b := time.Now()
			if err := runGitRetry(func() error {
				return runGit(tmpDir, "fetch",
					"--depth=1", "--no-tags",
					"--progress",
					"origin", *ref,
				)
			}, "fetch"); err != nil {
				fail("git fetch %s 失败: %v", *ref, err)
			}
			// 切换到指定 commit (detached HEAD)
			if err := runGit(tmpDir, "checkout", "--progress", *ref); err != nil {
				fail("git checkout %s 失败: %v", *ref, err)
			}
			log("Step 1b 完成 (%s)", time.Since(t0b))
		} else {
			// 分支/标签: 直接 clone --branch <ref>
			log("Step 1: git clone --depth=1 --branch %s", *ref)
			t0 = time.Now()
			if err := runGitRetry(func() error {
				return runGit("", "clone",
					"--depth=1", "--no-tags",
					"--branch", *ref,
					"--progress",
					*repo, tmpDir,
				)
			}, "clone"); err != nil {
				fail("git clone 失败: %v", err)
			}
			log("Step 1 完成 (%s)", time.Since(t0))
		}
	} else {
		// ---- 缓存命中: fetch + reset --hard 更新到指定版本 ----
		// fetch 拉取远端最新提交, 然后 reset --hard 把工作区对齐到目标版本.
		// fetch 失败不致命: 可能是离线运行, 继续用缓存旧版本.
		// 注意: 浅克隆 fetch 若被超时打断会残留 .git/shallow.lock, 导致后续 fetch 全部失败.
		//       fetch 前先清理残留锁文件.
		log("Step 1: 缓存复用, 拉取最新更新")
		t0 = time.Now()

		shallowLock := filepath.Join(tmpDir, ".git", "shallow.lock")
		if _, err := os.Stat(shallowLock); err == nil {
			os.Remove(shallowLock)
			log("  清理残留锁文件: .git/shallow.lock")
		}

		if err := runGitRetry(func() error {
			return runGit(tmpDir, "fetch",
				"--depth=1", "--no-tags",
				"--progress",
				"origin", *ref,
			)
		}, "fetch"); err != nil {
			log("  fetch 失败, 继续使用缓存旧版本: %v", err)
		} else {
			// fetch 成功, 把工作区对齐到目标版本.
			// - 分支/标签: reset --hard origin/<ref> (远端跟踪分支)
			// - commit SHA: reset --hard <sha> (fetch 已拉到该 commit)
			resetTarget := *ref
			if !isSHA {
				resetTarget = "origin/" + *ref
			}
			log("  git reset --hard %s", resetTarget)
			if err := runGit(tmpDir, "reset", "--hard", resetTarget); err != nil {
				// reset 失败可能是 ref 不存在或缓存损坏, 清除缓存提示重新运行
				log("  reset 失败, 清除缓存目录: %s", tmpDir)
				os.RemoveAll(tmpDir)
				fail("缓存已清除 (%s), 请重新运行 (reset --hard %s 失败: %v)", tmpDir, resetTarget, err)
			}
		}
		log("Step 1 完成 (%s)", time.Since(t0))
	}

	// Step 2: LFS pull (只拉取指定目录内的大文件, 带重试)
	// ⚠️ LFS 拉取失败会中断 (失败时大文件是 LFS 指针而非真实内容, 输出不可用).
	//    如需跳过 LFS (例如未安装 git-lfs 或不需要大文件), 使用 -no-lfs 参数.
	if !*noLFS && hasLFSFiles(tmpDir) {
		lfsIncludes := lfsIncludePatterns(dirList)
		// ⚠️ 多个模式必须用逗号拼接成单个字符串作为 --include 的值,
		//    不能作为多个独立参数传递 (否则 git 会把第 2 个起当作 remote 名, 报 "Invalid remote name")
		lfsIncludeArg := strings.Join(lfsIncludes, ",")
		log("Step 2: git lfs pull --include=%s", lfsIncludeArg)
		t2b := time.Now()
		runGit("", "lfs", "install")
		if err := runGitRetry(func() error {
			return runGit(tmpDir, "lfs", "pull", "--include", lfsIncludeArg)
		}, "lfs pull"); err != nil {
			fail("LFS pull 失败: %v (如未安装 git-lfs 或不需要大文件, 使用 -no-lfs 跳过)", err)
		}
		log("Step 2 完成 (%s)", time.Since(t2b))
	} else if *noLFS {
		log("Step 2: 跳过 LFS pull (-no-lfs)")
	}

	// Step 3: 拷贝目录到输出位置
	log("Step 3: 拷贝到输出目录")
	t3 := time.Now()
	for _, dir := range dirList {
		src := filepath.Join(tmpDir, dir)
		dst := filepath.Join(*output, dir)

		if _, err := os.Stat(src); os.IsNotExist(err) {
			fail("目录不存在: %s (请检查路径和 ref)", dir)
		}

		fmt.Printf("  拷贝 %s\n", dir)
		os.RemoveAll(dst)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			fail("创建目录失败: %v", err)
		}
		if err := copyDir(src, dst); err != nil {
			fail("拷贝失败 %s: %v", dir, err)
		}
	}
	log("Step 3 完成 (%s)", time.Since(t3))

	log("全部完成, 总耗时 %s", time.Since(t0))

	// Step 4: 清理过期缓存
	if ttl > 0 {
		cleanExpiredCache(*cacheDir, ttl)
	}
}

// cacheHash 根据 repo + ref 生成 12 位哈希作为缓存目录名
func cacheHash(repo, ref string) string {
	h := sha256.Sum256([]byte(repo + "|" + ref))
	return hex.EncodeToString(h[:])[:12]
}

// cleanExpiredCache 清理缓存目录中超过 TTL 的条目
func cleanExpiredCache(cacheDir string, ttl time.Duration) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-ttl)
	cleaned := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(cacheDir, entry.Name())
			os.RemoveAll(path)
			cleaned++
		}
	}
	if cleaned > 0 {
		log("清理过期缓存: %d 个 (> %s)", cleaned, ttl)
	}
}

// runGitRetry 包装网络操作, 支持超时检测 + 自动重试
func runGitRetry(fn func() error, opName string) error {
	var lastErr error
	for i := 0; i <= globalRetries; i++ {
		if i > 0 {
			log("  [%s] 重试 %d/%d", opName, i, globalRetries)
			time.Sleep(2 * time.Second)
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if lastErr == context.DeadlineExceeded {
			log("  [%s] 超时, 准备重试", opName)
		}
	}
	return lastErr
}

// hasLFSFiles 检查仓库是否使用了 Git LFS (.gitattributes 含 filter=lfs)
func hasLFSFiles(repoDir string) bool {
	attrsPath := filepath.Join(repoDir, ".gitattributes")
	data, err := os.ReadFile(attrsPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "filter=lfs")
}

// lfsIncludePatterns 生成 LFS --include 路径模式 (每目录加 /** 通配)
func lfsIncludePatterns(dirs []string) []string {
	var patterns []string
	for _, d := range dirs {
		patterns = append(patterns, d+"/**")
	}
	return patterns
}

// isCommitSHA 判断 ref 是否为 commit SHA (hex 字符串, 长度 >= 7)
func isCommitSHA(ref string) bool {
	if len(ref) < 7 {
		return false
	}
	for _, c := range ref {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// log 带时间戳的日志输出
func log(format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05")
	fmt.Printf("[%s] %s\n", ts, fmt.Sprintf(format, args...))
}

// durStr 格式化时长用于显示
func durStr(d time.Duration) string {
	if d == 0 {
		return "off"
	}
	return d.String()
}

func boolStr(b bool, trueVal, falseVal string) string {
	if b {
		return trueVal
	}
	return falseVal
}

// runGit 执行 git 命令, dir 非空时设置工作目录, 打印完整命令行
func runGit(dir string, args ...string) error {
	display := "git"
	if dir != "" {
		display += " -C " + dir
	}
	display += " " + strings.Join(args, " ")
	fmt.Fprintf(os.Stderr, "  $ %s\n", display)

	ctx := context.Background()
	var cancel context.CancelFunc
	if globalTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, globalTimeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// copyDir 递归拷贝目录, 保留文件权限
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}

		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()

		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		defer out.Close()

		_, err = io.Copy(out, in)
		return err
	})
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[FAIL] "+format+"\n", args...)
	os.Exit(1)
}
