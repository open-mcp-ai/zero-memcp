package embed

// Dot 是两个已归一化向量的相似度（内积 = 余弦）。
//
// 这个函数不带构建标签，因为它跟推理无关——纯 float32 加法。
// 早先它住在 embed.go（//go:build cgo）里，于是 CGO_ENABLED=0 时
// 整个函数消失，embed 包的测试连编译都过不去：
//
//	vet: internal/embed/embed_test.go:74 undefined: Dot
//
// 生产二进制不受影响（测试文件不参与构建），但「纯 Go 构建能过测试」
// 这条线断了——交叉编译到 windows/linux 之后没人能验证 embed 包。
// 数学不该被推理后端的构建标签绑架。
func Dot(a, b []float32) float32 {
	var s float32
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}
