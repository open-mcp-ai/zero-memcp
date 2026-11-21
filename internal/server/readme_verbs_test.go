package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// README 里「常用动词」表列出的每个动词，都必须真的注册在语法里。
//
// 为什么需要这条：文档腐烂不会报错。动词被改名或删除后，README 照旧
// 向人承诺它能用；照着敲的人得到的是一句"unknown verb"，然后去怀疑
// 自己记错了。文档与代码之间要有东西把它们钉住。
//
// 反向也检查：语法里注册了、README 说「常用」却没提，不报错——
// README 是选摘，不是全集；help --all 才是。
func TestReadmeVerbsExistInGrammar(t *testing.T) {
	readme := filepath.Join("..", "..", "README.md")
	raw, err := os.ReadFile(readme)
	if err != nil {
		t.Fatalf("读 README: %v", err)
	}
	section := extractSection(string(raw), "## 常用动词")
	if section == "" {
		t.Fatal("README 里找不到「## 常用动词」一节——测试与文档的锚点断了")
	}

	registered := map[string]bool{}
	for _, op := range zeroGrammar.Ops() {
		registered[op.Name] = true
	}

	// 表格首列形如 `| \`put\` | \`put [<tree>] $TEXT\` | ... |`，
	// 一行可能写多个（`log` / `ls`）。
	row := regexp.MustCompile(`^\|\s*(.+?)\s*\|`)
	code := regexp.MustCompile("`([a-z][a-z-]*)`")
	verbish := regexp.MustCompile(`^[a-z][a-z-]*$`)

	checked := 0
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "|") || strings.Contains(line, "---") {
			continue
		}
		m := row.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, tok := range code.FindAllStringSubmatch(m[1], -1) {
			name := tok[1]
			if !verbish.MatchString(name) {
				continue
			}
			// 表头与说明列里的普通词不算动词。
			if name == "verb" || name == "help" && !registered[name] {
				continue
			}
			if !registered[name] {
				t.Errorf("README 的「常用动词」表列了 %q，但语法里没有这个动词——文档在承诺一个不存在的能力", name)
				continue
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("没从 README 表格里解析出任何动词——正则或表格格式变了，这条测试已失去意义")
	}
	t.Logf("核对 %d 个 README 动词，全部存在于语法中", checked)
}

// extractSection 取 markdown 中某个二级标题到下一个同级标题之间的内容。
func extractSection(doc, heading string) string {
	i := strings.Index(doc, heading)
	if i < 0 {
		return ""
	}
	rest := doc[i+len(heading):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		return rest[:j]
	}
	return rest
}
