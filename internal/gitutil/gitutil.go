// Package gitutil 提供 gitsparse 各拉取模式共享的通用工具函数.
//
// 设计目标:
//   - 与具体拉取模式 (full / sparse / ...) 解耦, 仅含无状态的纯函数或简单结构
//   - 可被 internal/puller 下所有模式实现复用
//   - 可被测试单独覆盖
package gitutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
// 日志
// ============================================================================

// Logf 带时间戳的日志输出到 stdout.
func Logf(format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05")
	fmt.Printf("[%s] %s\n", ts, fmt.Sprintf(format, args...))
}

// Failf 输出错误到 stderr 并以退出码 1 退出.
func Failf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[FAIL] "+format+"\n", args...)
	os.Exit(1)
}

// DurStr 格式化时长用于显示; 0 显示为 "off".
func DurStr(d time.Duration) string {
	if d == 0 {
		return "off"
	}
	return d.String()
}

// BoolStr 返回 bool 的字符串表示.
func BoolStr(b bool, trueVal, falseVal string) string {
	if b {
		return trueVal
	}
	return falseVal
}

// ============================================================================
// 字符串 / ref 工具
// ============================================================================

// SplitAndTrim 按 sep 分割字符串并 trim 每段, 丢弃空串.
func SplitAndTrim(s, sep string) []string {
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

// IsCommitSHA 判断 ref 是否为 commit SHA (hex 字符串, 长度 >= 7).
func IsCommitSHA(ref string) bool {
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

// CacheHash 根据 repo + ref (+ 可选 extra) 生成 12 位 sha256 哈希作为缓存目录名.
// extra 用于隔离不兼容的缓存变体 (如 puller mode: full vs snip).
func CacheHash(repo, ref string, extra ...string) string {
	h := sha256.New()
	h.Write([]byte(repo))
	h.Write([]byte("|"))
	h.Write([]byte(ref))
	for _, e := range extra {
		h.Write([]byte("|"))
		h.Write([]byte(e))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// ============================================================================
// Git 版本解析与兼容性检测
// ============================================================================

// ParseGitVersion 从 "git version x.y.z" 格式的字符串解析版本号.
// 接受带或不带 "git version" 前缀的输入, 也兼容 "x.y.z.windows.1" 等后缀.
// 返回 [major, minor, patch] 三元组.
func ParseGitVersion(s string) ([3]int, error) {
	var ver [3]int
	// 去掉 "git version" 前缀
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "git version ")
	s = strings.TrimSpace(s)

	// 取第一段 x.y.z (忽略 .windows.1 等后缀)
	parts := strings.SplitN(s, " ", 2)
	core := parts[0]

	nums := strings.Split(core, ".")
	if len(nums) < 2 {
		return ver, fmt.Errorf("invalid git version: %q", s)
	}
	for i := 0; i < 3; i++ {
		if i >= len(nums) {
			break
		}
		// 去掉可能的非数字后缀 (如 "2.32.0-rc1")
		numStr := nums[i]
		for j, c := range numStr {
			if c < '0' || c > '9' {
				numStr = numStr[:j]
				break
			}
		}
		if numStr == "" {
			return ver, fmt.Errorf("invalid git version component: %q", nums[i])
		}
		n := 0
		for _, c := range numStr {
			n = n*10 + int(c-'0')
		}
		ver[i] = n
	}
	return ver, nil
}

// SupportsSparseCheckoutCone 判断给定 Git 版本是否支持 "sparse-checkout init --cone".
// cone 模式在 Git 2.25.0 引入.
func SupportsSparseCheckoutCone(ver [3]int) bool {
	return ver[0] > 2 || (ver[0] == 2 && ver[1] >= 25)
}

// IsValidRepo 检测 dir 是否为可用的 git 仓库.
// 用 `git rev-parse --git-dir` 判定, 比 stat .git 更权威:
// 上次 fresh 流程中断 (如 init 成功但 remote/fetch 失败) 会留下残缺 .git,
// stat .git 通过但 git 命令仍报 "not a git repository".
func IsValidRepo(dir string) bool {
	err := exec.Command("git", "-C", dir, "rev-parse", "--git-dir").Run()
	return err == nil
}

// ============================================================================
// Git 命令执行
// ============================================================================

// Runner 无状态的 git 命令执行器. 超时由每次 Run/RunRetry 调用时传 ctx 控制,
// 重试次数由 RunRetry 的 retries 参数控制. Runner 本身不再持任何配置.
type Runner struct{}

// Run 执行 git 命令, dir 非空时设置工作目录, 打印完整命令行.
// ctx 控制超时/取消: ctx 取消时子进程被杀.
func (r *Runner) Run(ctx context.Context, dir string, args ...string) error {
	display := "git"
	if dir != "" {
		display += " -C " + dir
	}
	display += " " + strings.Join(args, " ")
	fmt.Fprintf(os.Stderr, "  $ %s\n", display)

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// RunWithTimeout 是 Run 的便捷封装: timeout > 0 时派生带超时的子 ctx.
// 用于单命令超时控制 (替代旧 Runner.Timeout 字段).
func (r *Runner) RunWithTimeout(ctx context.Context, timeout time.Duration, dir string, args ...string) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return r.Run(ctx, dir, args...)
}

// RunRetry 包装网络操作, 支持超时检测 + 自动重试.
// 共重试 retries 次 (总尝试次数 = retries + 1).
// fn 接收 ctx (重试间复用同一 ctx) + attempt 索引: 0=首次, 1=第一次重试, 2+=第二次起.
// 调用方可按 attempt 决定是否在重试前清理残留状态 (如 fetch 中断留下的 .git/shallow.lock).
func (r *Runner) RunRetry(ctx context.Context, retries int, fn func(ctx context.Context, attempt int) error, opName string) error {
	var lastErr error
	for i := 0; i <= retries; i++ {
		if i > 0 {
			tag := ""
			if lastErr == context.DeadlineExceeded {
				tag = " (超时)"
			}
			Logf("  [%s] 重试 %d/%d (上次失败%s: %v)", opName, i, retries, tag, lastErr)
			time.Sleep(2 * time.Second)
		}
		lastErr = fn(ctx, i)
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// ============================================================================
// 文件系统工具
// ============================================================================

// PrepareCloneTarget 清理 clone 目标目录并确保父目录存在.
// 用于首次 clone 前以及重试前: git clone 超时/中断会残留部分写入的目录,
// 直接重试会因 "destination path already exists and is not an empty directory" 失败.
func PrepareCloneTarget(target string) {
	if _, err := os.Stat(target); err == nil {
		Logf("  清理目标目录: %s", target)
		os.RemoveAll(target)
	}
	os.MkdirAll(filepath.Dir(target), 0755)
}

// CopyDir 递归拷贝目录, 保留文件权限, 支持符号链接.
func CopyDir(src, dst string) error {
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

// CleanExpiredCache 清理缓存目录中超过 TTL 的条目.
func CleanExpiredCache(cacheDir string, ttl time.Duration) {
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
		Logf("清理过期缓存: %d 个 (> %s)", cleaned, ttl)
	}
}

// HasLFSFiles 检查仓库是否使用了 Git LFS (.gitattributes 含 filter=lfs).
func HasLFSFiles(repoDir string) bool {
	attrsPath := filepath.Join(repoDir, ".gitattributes")
	data, err := os.ReadFile(attrsPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "filter=lfs")
}

// PatternDirs 把文件 pattern 列表归约为去重的父目录列表 (供 sparse-checkout 等目录级操作用).
// "common/protocol/*.xml" → "common/protocol"; 根级 pattern ("*.md") 的父目录为 ".",
// 跳过 (cone 模式根文件默认全检出, 无需设置).
func PatternDirs(files []string) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, f := range files {
		d := filepath.Dir(f)
		if d == "." || seen[d] {
			continue
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	return dirs
}

// LFSIncludePatterns 生成 LFS --include 路径模式: 每目录加 "/**" 通配;
// 文件 pattern 原样追加 (LFS --include 原生支持 glob 语法, 如 "common/protocol/*.xml").
func LFSIncludePatterns(dirs, files []string) []string {
	var patterns []string
	for _, d := range dirs {
		patterns = append(patterns, d+"/**")
	}
	return append(patterns, files...)
}
