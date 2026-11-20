//go:build !cgo || !onnx

package embed

// 默认形态：不编译进程内 ONNX 推理。
//
// 触发条件有两个，缺一不可：
//   - 没有 CGO（跨平台交叉编译的常见情况）
//   - 没有显式加 -tags onnx
const onnxCompiled = false
