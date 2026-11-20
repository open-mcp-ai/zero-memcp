package server

import (
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// 当 SKILL.md 声明了 span 时，写入必须落在「日期/数字槽/文件名」三级路径下。
//
// 理由：
//   - 单目录文件数决定 tree 对象大小（不可变结构，写一次重写一次）
//   - 实测 thought/2026-11-11 = 3006 文件 / 163 KB，一天两千条写入
//     就是 300 MB 的 tree 反复重写
//   - 槽位按当天秒数除以 span 纯函数推导，给定时间就能算，不影响可读性
func TestSpanSlotSubdivision(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	// 给项目种一份带 span: 10m 的 SKILL.md（100755 模式，规则）
	skill := `---
name: zero
write:
  layout: date
  span: 10m
---
# 规则`
	repo, err := srv.projects.Ensure("blackegg/me")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write("zero", "SKILL.md", []byte(skill), gitstore.ModeRule); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("zero", "set-span"); err != nil {
		t.Fatal(err)
	}
	// Reload 让服务端重解析 SKILL.md 里的 policy
	if err := srv.Reload(); err != nil {
		t.Fatal(err)
	}

	// 写入一条，观察路径形状
	out, err := callMCP(t, srv, tok, "put blackegg/me -- 槽位测试记忆")
	if err != nil {
		t.Fatal(err)
	}

	// 路径必须形如：2026-11-19/XXX/HHMMSS.uuuuuu-seq-local.md
	// 其中 XXX 是三位数字槽号（000 ~ 143）
	today := time.Now().Format("2006-01-02")
	idx := strings.Index(out, today+"/")
	if idx < 0 {
		t.Fatalf("路径缺少当天日期：%s", out)
	}

	after := out[idx+len(today)+1:]
	parts := strings.Split(after, "/")
	if len(parts) < 2 {
		t.Fatalf("路径没有分槽（期望 日期/槽号/文件名，实际: %s）", out)
	}
	slot := parts[0]
	if len(slot) != 3 {
		t.Fatalf("槽号应为三位数字，实际 %q", slot)
	}
	for _, c := range slot {
		if c < '0' || c > '9' {
			t.Fatalf("槽号必须是全数字，实际 %q", slot)
		}
	}

	// 验证时间依然能被 sliceAtTime 正确解析（不丢事件时间）
	fields := strings.Fields(out)
	var fullPath string
	for _, f := range fields {
		if strings.Contains(f, today) {
			fullPath = strings.Trim(f, "(),")
			break
		}
	}
	if fullPath == "" {
		t.Fatalf("没找到完整路径：%s", out)
	}

	parsedTime := sliceAtTime("无 frontmatter 正文", fullPath)
	if parsedTime == 0 {
		t.Fatalf("新路径无法被 sliceAtTime 识别：%s", fullPath)
	}
	diff := time.Now().Unix() - parsedTime
	if diff < -10 || diff > 10 {
		t.Fatalf("从新路径抽出的时间和当前时间偏差过大：%d 秒", diff)
	}
}

// 没有声明 span 的项目必须保持旧行为（直接落在日期目录下，不分槽）。
// 这是向前兼容的底线：谁也没声明的时候不能擅自改路径。
func TestDefaultSpanKeepsLegacyLayout(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	out, err := callMCP(t, srv, tok, "put blackegg/me -- 默认布局测试")
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	idx := strings.Index(out, today+"/")
	if idx < 0 {
		t.Fatalf("路径缺少日期目录：%s", out)
	}
	after := out[idx+len(today)+1:]
	firstToken := strings.Fields(after)[0]
	firstToken = strings.Trim(firstToken, "(),")
	if strings.Contains(firstToken, "/") {
		t.Fatalf("默认布局不该出现二级槽目录（期望文件名，实际 %q）", firstToken)
	}
}
