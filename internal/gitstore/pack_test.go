package gitstore

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"
)

// 用真实的 git repack 把松散对象打包，再用我们的纯 Go 读取器读出来。
// 验证 packfile + delta 解包完全正确。
func TestPackReadCompatibility(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("跳过：需要 git 命令行制造 packfile")
	}
	dir := filepath.Join(t.TempDir(), "pack.git")
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 写多条有共性的内容，逼迫 git 使用 delta 压缩
	base := bytes.Repeat([]byte("这是一段很长很长很长的基准内容，用来产生增量压缩...\n"), 50)
	for i := 0; i < 5; i++ {
		data := append([]byte(nil), base...)
		data = append(data, []byte(string(rune('A'+i))+" 独特的改动行\n")...)
		if err := r.Write("zero", filepath.Join("dir", string(rune('a'+i))+".md"), data, ModeContent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Commit("zero", "准备打包"); err != nil {
		t.Fatal(err)
	}

	// 调 git repack -a -d：打成 packfile 并删掉所有松散对象
	cmd := exec.Command("git", "-C", dir, "repack", "-a", "-d", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("repack 失败: %v, out=%s", err, out)
	}

	// 确认松散对象全没了（只剩 pack）
	loosies, _ := filepath.Glob(filepath.Join(dir, "objects", "??", "*"))
	if len(loosies) > 0 {
		t.Fatalf("松散对象应全被删掉，还剩 %d 个", len(loosies))
	}
	packs, _ := filepath.Glob(filepath.Join(dir, "objects", "pack", "*.pack"))
	if len(packs) == 0 {
		t.Fatal("应该生成了 packfile")
	}

	// 重新 Open，强制走纯 Go pack 读取器
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := r2.List("zero")
	if err != nil {
		t.Fatalf("List 走 pack 失败: %v", err)
	}
	if len(list) != 5 {
		t.Fatalf("应列出 5 个文件，得到 %d", len(list))
	}

	// 读每一篇，验证内容毫发无损
	for i := 0; i < 5; i++ {
		p := filepath.Join("dir", string(rune('a'+i))+".md")
		got, err := r2.Read("zero", p)
		if err != nil {
			t.Fatalf("Read %s 走 pack 失败: %v", p, err)
		}
		wantTail := string(rune('A'+i)) + " 独特的改动行\n"
		if !bytes.HasSuffix(got, []byte(wantTail)) {
			t.Fatalf("Read %s 内容不对: %q", p, string(got[len(got)-30:]))
		}
	}
}
