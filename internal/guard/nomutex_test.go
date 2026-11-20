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

// TestNoMutexInSource 是全局无锁门禁。
//
// 判据很硬：源码里出现 sync.Mutex / sync.RWMutex 的**字段声明**，
// 这条测试就失败。
//
// 依据见 nomutex.go 的包注释：锁内做 I/O 造成的死锁，termcp 记了 10+ 次，
// zero-memcp 记了 1 次进程直接没掉（fatal error，没有 recover 机会）。
func TestNoMutexInSource(t *testing.T) {
	root := moduleRoot(t)
	var offenders []string

	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", ".ext-align":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// 门禁测试自己会提到 sync.Mutex 的类型名，跳过它。
		if strings.HasSuffix(path, "nomutex_test.go") {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ft, ok := n.(*ast.Field)
			if !ok {
				return true
			}
			sel, ok := ft.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if pkg.Name == "sync" && (sel.Sel.Name == "Mutex" || sel.Sel.Name == "RWMutex") {
				offenders = append(offenders, fset.Position(ft.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("发现 sync.Mutex/sync.RWMutex 声明，全局无锁门禁拒绝：\n  %s\n\n"+
			"替代方案（按优先级）：\n"+
			"  1. 读多写少的 map/slice → atomic.Pointer[不可变快照]（拷贝-修改-交换）\n"+
			"  2. 每 key 独立、无跨 key 不变量 → sync.Map\n"+
			"  3. 需要串行化的写 → 一槽 channel 令牌 + 超时\n"+
			"  4. 锁内做 I/O 的重活 → 单飞 channel 或 CAS 的 backgroundRunner\n"+
			"  5. 聚合可变结构 → 分块 + 原子发布（见 internal/vecindex）\n\n"+
			"依据：internal/guard/nomutex.go 的包注释",
			strings.Join(offenders, "\n  "))
	}
}

// moduleRoot 向上找到 go.mod 所在目录。
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("找不到 go.mod")
	return ""
}