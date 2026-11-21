//go:build !windows

package server

import "os"

// permsVerifiable 报告本平台能否用 Unix 权限位判断「私密文件是否过宽」。
//
// Unix 上可以：0600/0700 与实际访问控制一一对应。
func permsVerifiable() bool { return true }

// permTooOpen 判断权限位是否过宽（组或其他可写）。
//
// 密钥材料必须是 0600/0700：可写意味着任何同组进程都能替换密钥内容，
// 而密钥是信任链的根，被替换就等于整条链被替换。
func permTooOpen(m os.FileMode) bool { return m.Perm()&0o022 != 0 }

// permHint 给「权限过宽」的报错补一句可执行的动作。
func permHint() string { return "chmod 0600 (file) / 0700 (dir) to enable" }
