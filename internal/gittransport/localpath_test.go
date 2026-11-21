package gittransport

import "testing"

// Windows 绝对路径与 UNC 路径必须被识别为本地仓库。
//
// 实测故障（GitHub Actions windows-latest，2026-11-21）：
//
//	TestMergeVerbE2E: push seed: unsupported remote transport:
//	  C:\Users\RUNNER~1\AppData\Local\Temp\TestMergeVerbE2E1655435314\002\backup.git
//
// 报错措辞像是「协议不支持」，实际是路径形态没认出来——
// 整个本地远端同步在 Windows 上不可用。
func TestLocalPathRecognizesWindowsForms(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`C:\data\backup.git`, true},
		{`C:/data/backup.git`, true},
		{`D:\backup.git`, true},
		{`\\server\share\backup.git`, true},
		{`/tmp/backup.git`, true},
		{`./backup.git`, true},
		{`../backup.git`, true},
		{`file:///tmp/backup.git`, true},
		// 不能误判：带冒号的非本地形态
		{`ssh://host/repo.git`, false},
		{`https://host/repo.git`, false},
		{`user@host:path/repo.git`, false},
		{`host:repo.git`, false},
	}
	for _, c := range cases {
		if _, ok := LocalPath(c.in); ok != c.want {
			t.Errorf("LocalPath(%q) = %v, want %v", c.in, ok, c.want)
		}
	}
}
