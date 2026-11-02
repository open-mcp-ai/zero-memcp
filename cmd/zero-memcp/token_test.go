package main

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 签发时就必须挡住「挂载点比凭证更宽」。
//
// 不挡的后果不是多给权限，而是让运行期去猜：cap=r 的凭证配上
// cap=w 的挂载点，交集是空集，而空集在那套代码里另有含义。
// 矛盾配置应该在签发这一步被拒，而不是变成一个需要读三个文件
// 才能解释的权限判定。
func TestValidateMountCapRejectsWidening(t *testing.T) {
	cases := []struct {
		name    string
		credCap string
		mount   auth.MountPoint
		wantErr bool
	}{
		{"挂载点不写 cap", "r", auth.MountPoint{Project: "p", Cap: ""}, false},
		{"凭证不写 cap 视为 rwx", "", auth.MountPoint{Project: "p", Cap: "rwx"}, false},
		{"挂载点是子集", "rwx", auth.MountPoint{Project: "p", Cap: "r"}, false},
		{"完全相等", "rw", auth.MountPoint{Project: "p", Cap: "rw"}, false},
		{"挂载点超出", "r", auth.MountPoint{Project: "p", Cap: "w"}, true},
		{"挂载点部分超出", "rw", auth.MountPoint{Project: "p", Cap: "rx"}, true},
		{"非法能力字符", "rwx", auth.MountPoint{Project: "p", Cap: "rz"}, true},
	}
	for _, c := range cases {
		err := validateMountCap(c.credCap, c.mount)
		if c.wantErr && err == nil {
			t.Errorf("%s：应当报错，却通过了", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s：不该报错，得到 %v", c.name, err)
		}
	}
}

// 错误信息要说清是哪一项、为什么——排查挂载表问题全靠这句话。
func TestValidateMountCapErrorMentionsProject(t *testing.T) {
	err := validateMountCap("r", auth.MountPoint{Project: "blackegg/me", Cap: "w"})
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), "blackegg/me") {
		t.Errorf("错误信息里应当出现项目名，得到 %v", err)
	}
}
