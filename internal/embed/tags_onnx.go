//go:build cgo && onnx

package embed

// 显式选择编译进程内 ONNX 推理。
//
// 为什么不是「有 CGO 就自动带上」：带 ONNX 会引入对 libonnxruntime 的
// 链接依赖，交叉编译立刻变复杂（要先备好目标平台的头文件与库）。
// 默认不带，需要的人自己加标签——这是「默认最小、能力显式」。
const onnxCompiled = true
