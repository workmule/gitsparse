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
// 通过 ldflags 可在构建时覆盖: go build -ldflags "-X 'main.Version=v1.0.1'"
const Version = "v1.0.0"

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
	timeout := flag.String("timeout", "5m", "Timeout per network operation (clone/fetch/lfs); 0 = no timeout")
	retries := flag.Int("retries", 3, "Retry count for network operations")
	cacheDir := flag.String("cache-dir", filepath.Join(os.TempDir(), cachePrefix), "Cache directory for cloned repos")
	cacheTTL := flag.String("cache-ttl", "24h", "Cache TTL; entries older than this are cleaned up (0 = no cleanup)")
	noCache := flag.Bool("no-cache", false, "Skip cache, force fresh clone")
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

	// 生成缓存 key: repo + ref 的哈希 (dirs 不影响 clone, 只影响 sparse checkout)
	cacheKey := cacheHash(*repo, *ref)
	tmpDir := filepath.Join(*cacheDir, cacheKey)

	// 检查缓存是否可用
	cached := false
	if !*noCache {
		if info, err := os.Stat(filepath.Join(tmpDir, ".git")); err == nil && info.IsDir() {
			cached = true
			log("缓存命中: %s", tmpDir)
		}
	}

	if !cached {
		log("缓存未命中, 克隆到: %s", tmpDir)
	}

	// 检测 ref 类型: commit SHA 还是 branch/tag
	isSHA := isCommitSHA(*ref)
	var t0 time.Time

	// Step 1: Shallow clone (仅在未缓存时执行)
	if !cached {
		if isSHA {
			log("Step 1: git clone --depth=1 (默认分支)")
			t0 = time.Now()
			if err := runGitRetry(func() error {
				os.RemoveAll(tmpDir)
				os.MkdirAll(tmpDir, 0755)
				return runGit("", "clone",
					"--no-checkout", "--sparse",
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
			log("Step 1b 完成 (%s)", time.Since(t0b))
		} else {
			log("Step 1: git clone --depth=1 (ref=%s)", *ref)
			t0 = time.Now()
			if err := runGitRetry(func() error {
				os.RemoveAll(tmpDir)
				os.MkdirAll(tmpDir, 0755)
				return runGit("", "clone",
					"--no-checkout", "--sparse",
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
		log("Step 1: 跳过 clone (缓存复用)")
		t0 = time.Now()
	}

	// Step 2: 配置 sparse checkout (cone 模式, 每次都重新设置, 因为 dirs 可能不同)
	log("Step 2: git sparse-checkout set --cone %s", strings.Join(dirList, " "))
	t1 := time.Now()
	sparseArgs := append([]string{"-C", tmpDir, "sparse-checkout", "set", "--cone"}, dirList...)
	if err := runGit("", sparseArgs...); err != nil {
		fail("git sparse-checkout 失败: %v", err)
	}
	log("Step 2 完成 (%s)", time.Since(t1))

	// Step 3: checkout
	checkoutRef := *ref
	if isSHA {
		checkoutRef = "FETCH_HEAD"
	}
	log("Step 3: git checkout %s (sparse 检出 cone 内文件)", checkoutRef)
	t2 := time.Now()
	if err := runGit(tmpDir, "checkout", "--progress", checkoutRef); err != nil {
		// 缓存可能过期 (分支已更新), 清除缓存重试
		if cached {
			log("  checkout 失败, 清除缓存重试...")
			os.RemoveAll(tmpDir)
			cached = false
			// 重新 clone (递归处理简单起见直接 fail, 下次运行会重新克隆)
			fail("缓存已清除, 请重新运行")
		}
		fail("git checkout 失败: %v", err)
	}
	log("Step 3 完成 (%s)", time.Since(t2))

	// Step 3b: LFS pull (只拉取 sparse 目录内的大文件, 带重试)
	if hasLFSFiles(tmpDir) {
		lfsIncludes := lfsIncludePatterns(dirList)
		log("Step 3b: git lfs pull --include=%s", strings.Join(lfsIncludes, ","))
		t2b := time.Now()
		runGit("", "lfs", "install")
		lfsArgs := append([]string{"lfs", "pull", "--include"}, lfsIncludes...)
		if err := runGitRetry(func() error {
			return runGit(tmpDir, lfsArgs...)
		}, "lfs pull"); err != nil {
			log("  LFS pull 失败 (可能未安装 git-lfs): %v", err)
		}
		log("Step 3b 完成 (%s)", time.Since(t2b))
	}

	// Step 4: 拷贝目录到输出位置
	log("Step 4: 拷贝到输出目录")
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
	log("Step 4 完成 (%s)", time.Since(t3))

	log("全部完成, 总耗时 %s", time.Since(t0))

	// Step 5: 清理过期缓存
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
