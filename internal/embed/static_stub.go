//go:build !static

package embed

import "fmt"

// 未编译进静态查表嵌入时（默认形态）的空壳。
//
// 默认不带它的理由：它需要一份 20~30MB 的模型文件。二进制本身很小，
// 但「有没有这份文件」是部署选择，不是编译选择——把它做成标签，
// 是为了让「不打算用本地嵌入」的部署连这段代码都不必带。
type StaticConfig struct {
	ModelPath string
	Name      string
}

type Static struct{}

var errNoStatic = fmt.Errorf("this binary was not built with the pure-Go static embedder")

func OpenStatic(cfg StaticConfig) (*Static, error) {
	return nil, fmt.Errorf("%w: %s", errNoStatic, buildHint("static"))
}

func (s *Static) Dim() int                                 { return 0 }
func (s *Static) Name() string                             { return "" }
func (s *Static) Embed(string) ([]float32, error)          { return nil, errNoStatic }
func (s *Static) EmbedBatch([]string) ([][]float32, error) { return nil, errNoStatic }
func (s *Static) Close() error                             { return nil }
