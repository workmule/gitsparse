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

// CacheHash 根据 repo + ref 生成 12 位 sha256 哈希作为缓存目录名.
func CacheHash(repo, ref string) string {
	h := sha256.Sum256([]byte(repo + "|" + ref))
	return hex.EncodeToString(h[:])[:12]
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

// ============================================================================
// Git 命令执行
// ============================================================================

// Runner 封装 git 命令执行参数 (timeout / retries), 替代旧版全局变量.
// 所有拉取模式应持有同一个 Runner 实例, 保证 timeout / retries 一致.
type Runner struct {
	Timeout time.Duration
	Retries int
}

// Run 执行 git 命令, dir 非空时设置工作目录, 打印完整命令行.
// Timeout > 0 时对子进程施加超时.
func (r *Runner) Run(dir string, args ...string) error {
	display := "git"
	if dir != "" {
		display += " -C " + dir
	}
	display += " " + strings.Join(args, " ")
	fmt.Fprintf(os.Stderr, "  $ %s\n", display)

	ctx := context.Background()
	var cancel context.CancelFunc
	if r.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
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

// RunRetry 包装网络操作, 支持超时检测 + 自动重试.
// 共重试 Retries 次 (总尝试次数 = Retries + 1).
func (r *Runner) RunRetry(fn func() error, opName string) error {
	var lastErr error
	for i := 0; i <= r.Retries; i++ {
		if i > 0 {
			Logf("  [%s] 重试 %d/%d", opName, i, r.Retries)
			time.Sleep(2 * time.Second)
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if lastErr == context.DeadlineExceeded {
			Logf("  [%s] 超时, 准备重试", opName)
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

// LFSIncludePatterns 生成 LFS --include 路径模式 (每目录加 /** 通配).
func LFSIncludePatterns(dirs []string) []string {
	var patterns []string
	for _, d := range dirs {
		patterns = append(patterns, d+"/**")
	}
	return patterns
}
