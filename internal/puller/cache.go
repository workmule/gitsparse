package puller

import (
	"os"
	"path/filepath"
	"time"

	"github.com/workmule/gitsparse/internal/gitutil"
)

// ============================================================================
// Cache — 缓存命中检测与维护 (所有拉取模式通用)
// ============================================================================

// Cache 封装缓存目录的命中检测与维护操作, 供所有拉取模式复用.
// 各模式只需把仓库 clone/fetch 到 workDir, 缓存命中的判断交给 Cache.
type Cache struct {
	// NoCache true 时 Hit() 永远返回 false (强制重新克隆).
	NoCache bool
}

// Hit 检测 workDir 是否已是有效的 git 工作区缓存 (含 .git 目录).
// NoCache=true 时永远返回 false.
func (c Cache) Hit(workDir string) bool {
	if c.NoCache {
		gitutil.Logf("缓存已禁用 (-no-cache), 强制重新克隆")
		return false
	}
	info, err := os.Stat(filepath.Join(workDir, ".git"))
	if err == nil && info.IsDir() {
		gitutil.Logf("缓存命中: %s", workDir)
		return true
	}
	gitutil.Logf("缓存未命中, 克隆到: %s", workDir)
	return false
}

// CleanShallowLock 清理浅克隆 fetch 中断残留的 <workDir>/.git/shallow.lock.
// 浅克隆 fetch 超时/中断会残留该锁文件, 导致后续 fetch 全部失败.
func (c Cache) CleanShallowLock(workDir string) {
	lockPath := filepath.Join(workDir, ".git", "shallow.lock")
	if _, err := os.Stat(lockPath); err == nil {
		os.Remove(lockPath)
		gitutil.Logf("  清理残留锁文件: .git/shallow.lock")
	}
}

// CleanExpired 清理 cacheRoot 下超过 ttl 的缓存条目.
// ttl <= 0 时不清理.
func (c Cache) CleanExpired(cacheRoot string, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	gitutil.CleanExpiredCache(cacheRoot, ttl)
}

// ============================================================================
// CopyDirsToOutput — 拷贝目录到输出 (所有拉取模式通用)
// ============================================================================

// CopyDirsToOutput 把 srcRoot 下的 dirs 各子目录拷贝到 outputDir 下同名路径.
// 目标已存在则先删除再拷贝 (保证幂等). 源目录不存在则返回错误.
func CopyDirsToOutput(srcRoot, outputDir string, dirs []string) error {
	gitutil.Logf("Step 3: 拷贝到输出目录")
	t0 := time.Now()
	for _, dir := range dirs {
		src := filepath.Join(srcRoot, dir)
		dst := filepath.Join(outputDir, dir)

		if _, err := os.Stat(src); err != nil {
			return err
		}
		gitutil.Logf("  拷贝 %s", dir)
		os.RemoveAll(dst)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err := gitutil.CopyDir(src, dst); err != nil {
			return err
		}
	}
	gitutil.Logf("Step 3 完成 (%s)", time.Since(t0))
	return nil
}
