package server

import "fmt"

// tomlPath 把一个文件系统路径安全地放进 TOML 双引号字符串里。
//
// 为什么需要：测试里的配置模板是拼接出来的，Windows 的临时目录形如
// C:\Users\RUNNER~1\AppData\Local\Temp\...，直接插进去就成了
//
//	data = "C:\Users\RUNNER~1\..."
//
// 而 TOML 把 `\U` 当转义序列的开头，解析直接失败：
//
//	line 3: unsupported escape sequence \U
//
// 表现是三个测试在 windows runner 上红，而 linux/macos 全绿——产品代码
// 一点问题没有，只是测试不可移植。CI 红灯会掩盖以后的真问题，所以必须修。
func tomlPath(p string) string {
	// %q 产出 Go 的双引号字符串；TOML 基本字符串与它在这些字符上一致
	// （反斜杠、双引号、控制字符都用反斜杠转义）。
	return fmt.Sprintf("%q", p)
}
