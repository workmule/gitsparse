package main

import (
	"strings"
	"testing"
)

// resolveAutoMode 是 auto 模式的决策纯函数:
// 输入 `git --version` 的输出串, 返回实际使用的模式名 ("snip"/"full") 与决策原因 (用于日志).
// 行为规格:
//   - Git >= 2.25 (支持 sparse-checkout --cone) → snip
//   - Git < 2.25 或版本串解析失败 → 保守退回 full
func TestResolveAutoMode_HighGitVersion_ChoosesSnip(t *testing.T) {
	cases := []string{
		"git version 2.39.5\n",
		"git version 2.25.0\n",           // 边界: cone 引入版本, 含
		"git version 2.30.1.windows.1\n", // Windows 后缀
		"git version 2.32.0-rc1\n",       // 预发布后缀
		"git version 3.0.0\n",            // 主版本跨越
	}
	for _, in := range cases {
		mode, _ := resolveAutoMode(in)
		if mode != "snip" {
			t.Errorf("resolveAutoMode(%q) = %q, want %q", in, mode, "snip")
		}
	}
}

func TestResolveAutoMode_UnparseableVersion_ChoosesFull(t *testing.T) {
	cases := []string{
		"",
		"not a git version at all",
		"git version\n", // 缺版本号
	}
	for _, in := range cases {
		mode, reason := resolveAutoMode(in)
		if mode != "full" {
			t.Errorf("resolveAutoMode(%q) mode = %q, want %q", in, mode, "full")
		}
		// 原因不得伪造版本号 (如 "git 0.0.0 < 2.25"), 那会误导排查
		if strings.Contains(reason, "0.0.0") {
			t.Errorf("resolveAutoMode(%q) reason = %q, 不应包含伪造版本号 0.0.0", in, reason)
		}
	}
}

func TestResolveAutoMode_LowGitVersion_ChoosesFull(t *testing.T) {
	cases := []string{
		"git version 2.24.4\n", // 边界: 不含 2.25
		"git version 2.20.1\n",
		"git version 1.8.3.1\n",
	}
	for _, in := range cases {
		mode, _ := resolveAutoMode(in)
		if mode != "full" {
			t.Errorf("resolveAutoMode(%q) = %q, want %q", in, mode, "full")
		}
	}
}
