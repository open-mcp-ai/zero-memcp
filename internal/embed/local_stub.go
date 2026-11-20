//go:build !cgo || !onnx

// 未编译进进程内 ONNX 推理时，LocalONNX 只是一个会报错的空壳。
//
// 触发条件：没有 CGO，或没有显式加 -tags onnx（见 tags_onnx_off.go）。
//
// 这个文件存在的意义不是「支持无 CGO 部署」这种抽象目标，而是让
// 交叉编译（GOOS=linux/windows）能直接成功——此前 CGO 让 Mac 上
// 编不出 Linux 二进制，容器镜像构建只能进容器编译。
//
// 这种形态下嵌入全靠外接（Ollama / TEI / 任何 OpenAI 兼容服务），
// 这正是 zero-memcp 的默认形态：本体不做推理。
package embed

import (
	"errors"
	"fmt"
)

// Config 与 LocalONNX 在无 CGO 构建里保留空壳，让 provider 配置
// （[semantic] lib/model/vocab）仍能解析，只是启动时给出明确错误。
type Config struct {
	LibPath   string
	ModelPath string
	VocabPath string
}

type LocalONNX struct{}

var errNoCGO = errors.New("this binary was not built with in-process ONNX support")

func OpenLocal(cfg Config) (*LocalONNX, error) {
	return nil, fmt.Errorf("%w: %s", errNoCGO, buildHint("onnx"))
}

func (e *LocalONNX) Dim() int                                 { return 0 }
func (e *LocalONNX) Name() string                             { return "" }
func (e *LocalONNX) Embed(string) ([]float32, error)          { return nil, errNoCGO }
func (e *LocalONNX) EmbedBatch([]string) ([][]float32, error) { return nil, errNoCGO }
func (e *LocalONNX) Close() error                             { return nil }
