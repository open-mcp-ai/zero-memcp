package config

import (
	"strings"
	"testing"
)

// 端口挂载项是安全边界的一部分，配置写错必须在启动时被拒。
//
// 这一层以前完全不校验：写错的项目名、写错的 cap 字母、写错的
// 虚拟路径都会静默变成一个「永远不匹配」的挂载项，症状是
// 「这个端口什么都看不到」——一个需要读完 portpolicy.go
// 才能解释的现象。
func TestListenMountValidation(t *testing.T) {
	base := `
[server]
data = "/tmp/x"

[[user]]
name = "zero"
root = true

[[project]]
name = "proj"
owner = "zero"
mode = "rwx------"
`
	cases := []struct {
		name    string
		listen  string
		wantErr string
	}{
		{
			name:   "合法挂载",
			listen: "addr = \"127.0.0.1:1\"\nallow = [\"*\"]\nmount = [\"proj=/:rw@main\"]",
		},
		{
			name:   "根路径合法",
			listen: "addr = \"127.0.0.1:1\"\nallow = [\"*\"]\nmount = [\"proj=/\"]",
		},
		{
			name:    "cap 有非法字符",
			listen:  "addr = \"127.0.0.1:1\"\nallow = [\"*\"]\nmount = [\"proj=/:rz\"]",
			wantErr: "may only contain rwx",
		},
		{
			name:    "项目名带 ..",
			listen:  "addr = \"127.0.0.1:1\"\nallow = [\"*\"]\nmount = [\"../evil=/\"]",
			wantErr: "project name",
		},
		{
			name:    "虚拟路径带 ..",
			listen:  "addr = \"127.0.0.1:1\"\nallow = [\"*\"]\nmount = [\"proj=/a/../b\"]",
			wantErr: "virtual path",
		},
		{
			name:    "分支名带 ..",
			listen:  "addr = \"127.0.0.1:1\"\nallow = [\"*\"]\nmount = [\"proj=/:rw@../evil\"]",
			wantErr: "branch name",
		},
		{
			name:    "重复监听同一地址",
			listen:  "addr = \"127.0.0.1:1\"\nallow = [\"*\"]",
			wantErr: "duplicate [[listen]]",
		},
		{
			name:    "allow list missing",
			listen:  "addr = \"127.0.0.1:1\"",
			wantErr: "needs an allow list",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := base
			if c.name == "重复监听同一地址" {
				cfg += "\n[[listen]]\n" + c.listen + "\n[[listen]]\n" + c.listen + "\n"
			} else {
				cfg += "\n[[listen]]\n" + c.listen + "\n"
			}
			f, err := ParseFile([]byte(cfg))
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("不该报错: %v", err)
				}
				if verr := f.Validate(); verr != nil {
					t.Fatalf("Validate 不该报错: %v", verr)
				}
				return
			}
			// 错误可能出现在解析或校验任一步，两处都接受——
			// 重要的是「它被拒了，且理由里说得清是哪一项」。
			var got string
			if err != nil {
				got = err.Error()
			} else if verr := f.Validate(); verr != nil {
				got = verr.Error()
			}
			if got == "" {
				t.Fatalf("应当报错（含 %q），却通过了", c.wantErr)
			}
			if !strings.Contains(got, c.wantErr) {
				t.Errorf("错误信息应含 %q，得到 %q", c.wantErr, got)
			}
		})
	}
}
