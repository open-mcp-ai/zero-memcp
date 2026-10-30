package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// JTILen 是 jti 的十六进制字符数（8 字节 = 64 位，碰撞概率可忽略）。
const JTILen = 16

// NewJTI 生成一张凭证的唯一标识。
func NewJTI() (string, error) {
	b := make([]byte, JTILen/2)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成 jti: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Revocations 是吊销名单：一个 jti 一个空文件。
//
// 用目录而不是列表文件，理由是读写频率严重不对称：
// 吊销是几个月一次的运维动作，而校验是每一次请求都要做的。
// 目录的存在性由内核 stat 完成——不需要解析文件、不需要处理并发写、
// 不需要担心列表文件被写坏导致服务起不来。
//
// 删掉一个文件即撤销吊销，语义直觉，且无法出现「列表里少写了一行
// 所以本该被吊销的凭证还活着」这类静默失效。
type Revocations struct {
	dir string
}

// NewRevocations 绑定一个吊销目录。dir 为空时一切操作都是空操作。
func NewRevocations(dir string) *Revocations { return &Revocations{dir: dir} }

func (r *Revocations) path(jti string) string { return filepath.Join(r.dir, jti) }

// parentsDir 是吊销目录的兄弟目录，记录每张凭证的签发者。
//
// 放兄弟目录而不是子目录：List() 靠 ReadDir 列吊销名单，
// 子目录会混进去，而混进去的后果是「列出来一个不存在的东西」
// 或者更糟——把 parent 记录当成吊销标记。
func (r *Revocations) parentsDir() string {
	return filepath.Join(filepath.Dir(r.dir), "parents")
}

// RecordParent 记录 child 是由 parent 签发的。
//
// 没有这条记录，级联吊销就无从谈起：父凭证一废，派生出的一切
// 依然是有效凭证，等于吊销了个寂寞。
func (r *Revocations) RecordParent(child, parent string) error {
	if r == nil || r.dir == "" || child == "" || parent == "" {
		return nil
	}
	if err := ValidateJTI(child); err != nil {
		return err
	}
	if err := ValidateJTI(parent); err != nil {
		return err
	}
	dir := r.parentsDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("建父链目录 %s: %w", dir, err)
	}
	return os.WriteFile(filepath.Join(dir, child), []byte(parent), 0o444)
}

// parentOf 读出某张凭证的签发者，没有就返回空串。
func (r *Revocations) parentOf(jti string) string {
	b, err := os.ReadFile(filepath.Join(r.parentsDir(), jti))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Revoked 报告该 jti 是否已被吊销，**沿父链向上追溯**。
//
// 空 jti 视为未吊销：老版本签发的凭证没有这个字段，
// 把「没有编号」等同于「已作废」会让升级瞬间踢掉所有设备。
//
// 深度上限 32：父链本应是树，但记录是可写的文件，
// 一个手滑写出的环会让这里死循环——而它是每个请求都要走的路径。
func (r *Revocations) Revoked(jti string) bool {
	if r == nil || r.dir == "" || jti == "" {
		return false
	}
	for depth := 0; jti != "" && depth < 32; depth++ {
		if _, err := os.Stat(r.path(jti)); err == nil {
			return true
		}
		jti = r.parentOf(jti)
	}
	return false
}

// Revoke 把 jti 加入名单。重复吊销视为成功。
func (r *Revocations) Revoke(jti string) error {
	if err := ValidateJTI(jti); err != nil {
		return err
	}
	if r.dir == "" {
		return fmt.Errorf("未配置吊销目录")
	}
	if err := os.MkdirAll(r.dir, 0o750); err != nil {
		return fmt.Errorf("建吊销目录 %s: %w", r.dir, err)
	}
	f, err := os.OpenFile(r.path(jti), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("写吊销标记 %s: %w", jti, err)
	}
	return f.Close()
}

// Unrevoke 撤销一条吊销记录（删文件）。
func (r *Revocations) Unrevoke(jti string) error {
	if err := ValidateJTI(jti); err != nil {
		return err
	}
	if err := os.Remove(r.path(jti)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除吊销标记 %s: %w", jti, err)
	}
	return nil
}

// List 返回全部已吊销的 jti，按字典序。
func (r *Revocations) List() ([]string, error) {
	if r.dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ValidateJTI 校验 jti 的形态。它同时是文件名，所以必须挡掉路径分隔符。
func ValidateJTI(jti string) error {
	if len(jti) != JTILen {
		return fmt.Errorf("jti 应为 %d 位十六进制，得到 %q", JTILen, jti)
	}
	for _, c := range jti {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return fmt.Errorf("jti 只能是小写十六进制，得到 %q", jti)
		}
	}
	return nil
}
