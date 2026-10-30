package server

import (
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

func testMount() *Mount {
	return NewMount([]auth.MountPoint{
		{Project: "blackegg/me", At: "/", Branch: "zero"},
		{Project: "shared/common", At: "/shared", Branch: "zero"},
	}, "zero")
}

func TestMountToReal(t *testing.T) {
	m := testMount()

	cases := []struct {
		in      string
		project string
		path    string
		wantErr bool
	}{
		{"01-shape.md", "blackegg/me", "01-shape.md", false},
		{"SKILL.md", "blackegg/me", "SKILL.md", false},
		{"2026-10-30/x.md", "blackegg/me", "2026-10-30/x.md", false},
		// 子挂载：/shared 必须比 / 优先
		{"shared/x.md", "shared/common", "x.md", false},
		{"shared/a/b.md", "shared/common", "a/b.md", false},
		// 落在根挂载上的任何东西，都只是根仓库里的相对路径。
		{"other/repo/x.md", "blackegg/me", "other/repo/x.md", false},
	}

	for _, c := range cases {
		p, _, path, err := m.ToReal(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ToReal(%q) 应报错", c.in)
			}
			continue
		}
		if err != nil || p != c.project || path != c.path {
			t.Errorf("ToReal(%q) = (%q,%q,%v)，要 (%q,%q)", c.in, p, path, err, c.project, c.path)
		}
	}
}

func TestMountToVirtual(t *testing.T) {
	m := testMount()

	cases := []struct{ project, path, want string }{
		{"blackegg/me", "01-shape.md", "01-shape.md"},
		{"blackegg/me", "2026-10-30/x.md", "2026-10-30/x.md"},
		{"shared/common", "x.md", "shared/x.md"},
		// 不在表里 → 空串
		{"other/repo", "x.md", ""},
	}
	for _, c := range cases {
		if got := m.ToVirtual(c.project, "zero", c.path); got != c.want {
			t.Errorf("ToVirtual(%q,%q) = %q，要 %q", c.project, c.path, got, c.want)
		}
	}
}

// 同一个仓库挂两个分支：这是「同时读写不同分支」的核心。
func TestMountSameRepoTwoBranches(t *testing.T) {
	m := NewMount([]auth.MountPoint{
		{Project: "daily", At: "/", Branch: "chat", Cap: "rw"},
		{Project: "daily", At: "/main", Branch: "zero", Cap: "r"},
	}, "zero")

	// 根挂 chat 分支
	p, br, path, err := m.ToReal("2026-10-30/a.md")
	if err != nil || p != "daily" || br != "chat" || path != "2026-10-30/a.md" {
		t.Fatalf("根应映射到 daily@chat，得到 (%q,%q,%q,%v)", p, br, path, err)
	}
	// /main 挂 zero 分支
	p, br, path, err = m.ToReal("main/2026-10-30/a.md")
	if err != nil || p != "daily" || br != "zero" || path != "2026-10-30/a.md" {
		t.Fatalf("/main 应映射到 daily@zero，得到 (%q,%q,%q,%v)", p, br, path, err)
	}
	// 两个分支的能力不同
	if got := m.CapOf("daily", "chat"); got != "rw" {
		t.Errorf("chat 分支 cap 应为 rw，得到 %q", got)
	}
	if got := m.CapOf("daily", "zero"); got != "r" {
		t.Errorf("zero 分支 cap 应为 r，得到 %q", got)
	}
	// 输出侧：两个分支翻成不同虚拟路径
	if got := m.ToVirtual("daily", "chat", "x.md"); got != "x.md" {
		t.Errorf("chat 分支应翻成 x.md，得到 %q", got)
	}
	if got := m.ToVirtual("daily", "zero", "x.md"); got != "main/x.md" {
		t.Errorf("zero 分支应翻成 main/x.md，得到 %q", got)
	}
}

// 没有根挂载时，表外路径必须报错，绝不猜。
func TestMountNoRootRejects(t *testing.T) {
	m := NewMount([]auth.MountPoint{{Project: "blackegg/me", At: "/me"}}, "zero")
	if _, _, _, err := m.ToReal("elsewhere/x.md"); err == nil {
		t.Fatal("表外路径应报错")
	}
	if p, _, path, err := m.ToReal("me/x.md"); err != nil || p != "blackegg/me" || path != "x.md" {
		t.Fatalf("表内路径应命中，得到 (%q,%q,%v)", p, path, err)
	}
}

// 空挂载表 = 全命名空间，两端都不改写。
func TestMountEmptyPassthrough(t *testing.T) {
	var m *Mount
	if !m.Empty() {
		t.Fatal("nil 应视为空")
	}
	if got := m.ToVirtual("a/b", "zero", "c.md"); got != "a/b/c.md" {
		t.Errorf("空表应原样返回，得到 %q", got)
	}
	if !m.Visible("anything", "zero") {
		t.Error("空表应全部可见")
	}
}

func TestMountVisible(t *testing.T) {
	m := NewMount([]auth.MountPoint{{Project: "blackegg/me", At: "/", Branch: "zero"}}, "zero")
	if !m.Visible("blackegg/me", "zero") {
		t.Error("表内 (项目,分支) 应可见")
	}
	if m.Visible("blackegg/daily", "zero") {
		t.Error("表外项目必须不可见")
	}
	if m.Visible("blackegg/me", "other") {
		t.Error("表外分支必须不可见")
	}
}

// 往返一致。
func TestMountRoundTrip(t *testing.T) {
	m := testMount()
	for _, c := range []struct{ project, path string }{
		{"blackegg/me", "01.md"},
		{"blackegg/me", "a/b/c.md"},
		{"shared/common", "x.md"},
	} {
		virt := m.ToVirtual(c.project, "zero", c.path)
		proj, _, path, err := m.ToReal(virt)
		if err != nil || proj != c.project || path != c.path {
			t.Errorf("往返不一致: %q/%q → %q → (%q,%q,%v)", c.project, c.path, virt, proj, path, err)
		}
	}
}
