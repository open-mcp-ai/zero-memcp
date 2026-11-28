package guard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 「字段声明了、却从来没人赋值」是一类静默失败。
//
// 实例（2026-11-28，issue #14）：syncRecord.DurationMs 在 synclog.go 里
// 声明成 `json:"duration_ms"`，而生产代码里没有任何赋值点。于是每条同步
// 流水都写着 `"duration_ms": 0`——**字段在、值永远为零**。看起来像「同步
// 快到 0 毫秒」，实际是一次 10,007 条的推送跑了 12.5 分钟。
//
// 危害不是那个数字，是它让人失去唯一的度量手段：当时只能拿相邻两条记录
// 的时间差去猜，猜错了一轮。这类 bug 编译不报、运行不报、测试不报，
// 只有人去查的时候才发现「原来你从来没填过」。
//
// 所以把它做成门禁：DurationMs 必须在生产代码里被赋值。
// 断言的是「有赋值点」，不是「赋值正确」——正确性归行为测试，
// 这里只保证「不会永远为零」。
func TestDurationMsIsActuallyAssigned(t *testing.T) {
	root := moduleRoot(t)
	assigned := 0
	declared := 0

	err := filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		// 测试文件不算：测试里给字段赋值不能证明生产路径会填它。
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			id, ok := kv.Key.(*ast.Ident)
			if !ok || id.Name != "DurationMs" {
				return true
			}
			// 只有出现在结构体字面量里的 DurationMs 才算赋值。
			// 声明处是 *ast.Field，不会被这里命中，所以不用额外排除。
			declared++
			// 值不能是常量 0：那等于没填。
			if bl, isBasic := kv.Value.(*ast.BasicLit); isBasic && bl.Value == "0" {
				t.Errorf("%s: DurationMs 被赋成常量 0，等于没填", fset.Position(kv.Pos()))
				return true
			}
			assigned++
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if assigned == 0 {
		t.Fatalf("生产代码里找不到任何 DurationMs 赋值点（扫到 %d 处字面量）："+
			"同步流水会永远写 duration_ms=0，让人无法判断一次同步到底跑了多久。"+
			"见 issue #14。", declared)
	}
}
