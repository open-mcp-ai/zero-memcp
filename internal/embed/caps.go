package embed

import "strings"

// Capabilities 描述**这个二进制文件**编译进了哪些嵌入能力。
//
// 为什么需要它：能力是编译期决定的，而配置是运行期给的。两者错配的
// 时候必须当场说清楚「为什么用不了、怎么才能用上」——否则用户看到的
// 只是「provider 打不开」，然后去怀疑配置、怀疑模型、怀疑网络。
//
// 三种能力的代价完全不同：
//
//	http   —— 永远有。只需一个 URL，零依赖、零 CGO、零模型文件。
//	static —— 永远有。纯 Go 查表引擎，约 200 行、零依赖；**模型文件**
//	          才是可选的运行时输入（model_path），代码本身没有理由让人
//	          为它重新编译一次。放得下就常驻，用得着才配 model_path。
//	onnx   —— 需要显式编译（-tags onnx && CGO_ENABLED=1）。它要链接
//	          libonnxruntime 动态库，交叉编译会变麻烦——这是唯一一个
//	          「代码本身有部署代价」的引擎，所以只有它需要开关。
type Capabilities struct {
	HTTP   bool
	Static bool
	ONNX   bool
}

// staticCompiled 恒为真：纯 Go 查表引擎没有部署代价，没必要做成开关。
// 它的「可选性」体现在运行时是否配了 model_path，而不是是否编进了二进制。
const staticCompiled = true

// Compiled 返回本二进制的编译期能力。
func Compiled() Capabilities {
	return Capabilities{HTTP: true, Static: staticCompiled, ONNX: onnxCompiled}
}

// String 输出给 status / --version 的单行描述。
//
// 写成「http (always) | static (compiled) | onnx (not compiled)」而不是
// 一串 true/false：运维要的是「能干什么」，不是布尔代数。
func (c Capabilities) String() string {
	parts := make([]string, 0, 3)
	parts = append(parts, "http (always)")
	if c.Static {
		parts = append(parts, "static (compiled)")
	} else {
		parts = append(parts, "static (not compiled)")
	}
	if c.ONNX {
		parts = append(parts, "onnx (compiled)")
	} else {
		parts = append(parts, "onnx (not compiled)")
	}
	return strings.Join(parts, " | ")
}

// CapabilityLine 是 status 与 --version 共用的一行。
func CapabilityLine() string { return "embed capabilities: " + Compiled().String() }

// buildHint 给出「想用某个能力该怎么编」的确切命令。
//
// 报错里必须带命令：只说「不支持」等于把人留在原地。
func buildHint(kind string) string {
	switch strings.ToLower(kind) {
	case "onnx":
		return `rebuild with: CGO_ENABLED=1 go build -tags onnx ./cmd/zero-memcp ` +
			`(or switch this provider to kind="http")`
	case "static":
		return `no rebuild needed: kind="static" is always available; ` +
			`check that model_path points at a valid .zmstatic file`
	default:
		return "supported kinds: http (always), static (-tags static), onnx (-tags onnx with CGO)"
	}
}
