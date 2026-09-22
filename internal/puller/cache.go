package puller

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// Hit 检测 workDir 是否已是有效的 git 工作区缓存.
// 用 gitutil.IsValidRepo 验证 .git 可用 (非仅存在): 上次 fresh 中断会留下残缺 .git,
// stat 通过但 git 命令报 "not a git repository", 必须重新 fresh.
// NoCache=true 时永远返回 false.
func (c Cache) Hit(workDir string) bool {
	if c.NoCache {
		gitutil.Logf("缓存已禁用 (-no-cache), 强制重新克隆")
		return false
	}
	if !gitutil.IsValidRepo(workDir) {
		gitutil.Logf("缓存未命中或已损坏, 克隆到: %s", workDir)
		return false
	}
	gitutil.Logf("缓存命中: %s", workDir)
	return true
}

// CleanShallowLock 清理浅克隆 fetch 中断残留的 <workDir>/.git/shallow.lock.
// 浅克隆 fetch 超时/中断会残留该锁文件, 导致后续 fetch 全部失败.
//
// ponytail: 已被 CleanGitLocks 覆盖, 保留作向后兼容 (调用方未迁移时仍可用).
func (c Cache) CleanShallowLock(workDir string) {
	c.CleanGitLocks(workDir)
}

// CleanGitLocks 清理 <workDir>/.git/*.lock 残留锁文件.
// git 命令超时/中断会留下锁: fetch 留 shallow.lock, reset/checkout 留 index.lock, 等.
// 这些锁会导致后续 git 命令报 "Unable to create '...lock': File exists" 而失败.
func (c Cache) CleanGitLocks(workDir string) {
	gitDir := filepath.Join(workDir, ".git")
	entries, err := os.ReadDir(gitDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".lock") {
			continue
		}
		lockPath := filepath.Join(gitDir, name)
		if err := os.Remove(lockPath); err == nil {
			gitutil.Logf("  清理残留锁文件: .git/%s", name)
		}
	}
}

// ============================================================================
// CopyFilesToOutput — 按 glob 拷贝文件到输出 (所有拉取模式通用)
// ============================================================================

// CopyFilesToOutput 把 srcRoot 下匹配 files glob 的文件/目录拷贝到 outputDir 对应路径
// (保持目录结构, 如 "common/protocol/*.xml" → <output>/common/protocol/xxx.xml).
// 匹配项若已位于 dirs (-dirs) 中某目录内则跳过, 避免与整目录拷贝重复.
// glob 0 匹配时: skipMissing=true 跳过该 pattern, 否则返回错误.
func CopyFilesToOutput(srcRoot, outputDir string, files, dirs []string, skipMissing bool) error {
	if len(files) == 0 {
		return nil
	}
	gitutil.Logf("Step 3: 拷贝匹配文件到输出 (%d 个 pattern)", len(files))
	t0 := time.Now()
	for _, pat := range files {
		matches, err := filepath.Glob(filepath.Join(srcRoot, pat))
		if err != nil {
			return fmt.Errorf("无效的 pattern %q: %w", pat, err)
		}
		if len(matches) == 0 {
			if skipMissing {
				gitutil.Logf("  跳过 %s (无匹配文件)", pat)
				continue
			}
			return fmt.Errorf("pattern %q 没有匹配到任何文件", pat)
		}
		for _, src := range matches {
			rel, err := filepath.Rel(srcRoot, src)
			if err != nil {
				return err
			}
			if underAnyDir(rel, dirs) {
				continue // 已被 -dirs 整目录拷贝覆盖
			}
			if err := copyPath(src, filepath.Join(outputDir, rel)); err != nil {
				return err
			}
		}
	}
	gitutil.Logf("Step 3 完成 (%s)", time.Since(t0))
	return nil
}

// underAnyDir 判断 rel 路径是否位于 dirs 中任一目录内 (等于或为其子路径).
func underAnyDir(rel string, dirs []string) bool {
	for _, d := range dirs {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// copyPath 拷贝单个文件 (保持父目录) 或整目录 (目标先删再拷, 幂等) 到 dst.
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		os.RemoveAll(dst)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		return gitutil.CopyDir(src, dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
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
// 目标已存在则先删除再拷贝 (保证幂等). 源目录不存在时:
// skipMissing=true 跳过该目录继续, 否则返回错误.
func CopyDirsToOutput(srcRoot, outputDir string, dirs []string, skipMissing bool) error {
	if len(dirs) == 0 {
		return nil
	}
	gitutil.Logf("Step 3: 拷贝目录到输出")
	t0 := time.Now()
	for _, dir := range dirs {
		src := filepath.Join(srcRoot, dir)
		dst := filepath.Join(outputDir, dir)

		if _, err := os.Stat(src); err != nil {
			if skipMissing && os.IsNotExist(err) {
				gitutil.Logf("  跳过 %s (源目录不存在)", dir)
				continue
			}
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
