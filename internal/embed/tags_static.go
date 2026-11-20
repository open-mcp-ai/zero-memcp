//go:build static

package embed

// 显式选择编译纯 Go 静态查表嵌入。
//
// 它是 CGO_ENABLED=0 也能用的本地推理：模型是一张预先算好的词向量表，
// 运行时只做「查表 + 平均 + 归一化」，没有矩阵乘法、没有动态库依赖。
const staticCompiled = true
