//go:build windows

package server

import "os"

// permsVerifiable 报告本平台能否用 Unix 权限位判断「私密文件是否过宽」。
//
// Windows 上不能：Mode().Perm() 由 Go 按只读属性合成，普通文件恒为 0666、
// 目录恒为 0777，与实际 ACL 无关。照搬 Unix 的 `perm&0o022 != 0` 判断，
// 结果是**每一个密钥文件都被拒绝**——密钥热投放功能在 Windows 上
// 完全不可用，而且只在日志里留一行 REFUSING，看起来像用户配错了权限。
//
// 实测（GitHub Actions windows-latest，2026-11-21）：
//
//	TestKeysDirectoryHotdrop: keys/ 目录下 2 把钥匙应当全部生效，实际 loaded: 0
//
// 这不是放宽标准，是承认「这个平台没有这项信息」。真实保护要靠
// NTFS ACL，那是另一套机制，不能假装 Unix 权限位验过了。
func permsVerifiable() bool { return false }

// permTooOpen 在 Windows 上恒为 false：没有可用的权限位信息，
// 不能据此拒绝合法密钥。调用方必须先看 permsVerifiable()。
func permTooOpen(m os.FileMode) bool { return false }

// permHint 在 Windows 上指向真正的机制，而不是 chmod。
func permHint() string { return "restrict via NTFS ACL (icacls) — Unix mode bits are not meaningful on Windows" }
