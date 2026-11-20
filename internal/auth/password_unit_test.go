package auth

import (
	"testing"
)

func TestPasswordStore(t *testing.T) {
	dir := t.TempDir()
	ps := NewPasswordStore(dir)

	if ps.Exists() {
		t.Fatal("初始时不应存在密码")
	}

	// 首次启动：显式指定初始密码
	plain, gen, err := ps.Bootstrap("init-pass-1234")
	if err != nil {
		t.Fatal(err)
	}
	// generated 的语义是「发生了一次引导、口令必须被换掉」，不是
	// 「随机生成」：显式指定口令时同样要求改密——写在 env / 部署配置里
	// 的口令迟早会留在那些文件里。
	if !gen {
		t.Fatal("首次引导时 generated 应当为 true（要求改密）")
	}
	if plain != "" {
		t.Fatalf("显式指定口令时不该回显，实际返回 %q", plain)
	}
	if !ps.Exists() {
		t.Fatal("Bootstrap 后应当存在密码")
	}
	if !ps.MustChange() {
		t.Fatal("初始 Bootstrap 后应当要求改密")
	}

	// 校验 pending 密码
	if !ps.Verify("init-pass-1234") {
		t.Fatal("正确的 pending 密码应当校验通过")
	}
	if ps.Verify("wrong-password") {
		t.Fatal("错误密码应当失败")
	}

	// 正式改密
	if err := ps.Set("new-strong-password"); err != nil {
		t.Fatal(err)
	}
	if ps.MustChange() {
		t.Fatal("改密后不应再要求改密")
	}
	if !ps.Verify("new-strong-password") {
		t.Fatal("新密码应当校验通过")
	}
	if ps.Verify("init-pass-1234") {
		t.Fatal("旧 pending 密码应当失效")
	}

	// 重启后状态保持
	ps2 := NewPasswordStore(dir)
	if !ps2.Exists() {
		t.Fatal("重启后应当存在密码")
	}
	if ps2.MustChange() {
		t.Fatal("改过密之后重启不应要求再改")
	}
	if !ps2.Verify("new-strong-password") {
		t.Fatal("重启后新密码仍应有效")
	}
}

// 自动生成初始密码分支
func TestPasswordStoreBootstrapAutoGenerate(t *testing.T) {
	dir := t.TempDir()
	ps := NewPasswordStore(dir)

	plain, gen, err := ps.Bootstrap("")
	if err != nil {
		t.Fatal(err)
	}
	if !gen {
		t.Fatal("未显式给密码时应当自动生成")
	}
	if len(plain) == 0 {
		t.Fatal("生成的密码不应为空")
	}
	if !ps.Verify(plain) {
		t.Fatal("生成的密码应当能通过验证")
	}
}
