package embed

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

// Revisioned 是可选的「修订号」能力。
//
// 不是所有后端都实现它：远端 HTTP 后端不知道该服务跑的是哪份权重，
// 这种情况由配置里的 model_rev 显式声明。实现者把**自己确知的**信息
// 报出来（例如本地模型文件的哈希），比让用户手抄一长串哈希可靠。
type Revisioned interface {
	Rev() string
}

// Revision 汇总一个 Embedder 的修订号；没有就返回空串。
//
// 空串的含义是「未声明」，它和任何非空值都不相等——于是从旧版本升级
// 上来会触发一次重建。宁可多算一次，也不混两个向量空间。
func Revision(e Embedder) string {
	if e == nil {
		return ""
	}
	if r, ok := e.(Revisioned); ok {
		return r.Rev()
	}
	return ""
}

// ChainRev 把链上各后端的修订号合成一个值。
//
// 链降级时换了后端就等于换了权重，所以整条链的身份必须包含每一段。
func ChainRev(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p)
		b.WriteByte('\x00')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// FileRev 计算模型文件的短哈希，作为「同一模型名下的权重修订号」。
//
// 用它而不是 mtime：文件被复制、被 touch、被 rsync 都不会改内容，
// 用 mtime 会平白触发全量重建。
func FileRev(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:8])
}
