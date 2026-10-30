package manifest

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	doc := `---
name: my-notes
write:
  target: notes
  layout: flat
  kinds: [fact, thought]
---

# 规则正文
`
	p := Parse("default-proj", []byte(doc))
	if p.Name != "my-notes" {
		t.Errorf("Name = %q, want my-notes", p.Name)
	}
	if p.Target != "notes" {
		t.Errorf("Target = %q, want notes", p.Target)
	}
	if p.Layout != "flat" {
		t.Errorf("Layout = %q, want flat", p.Layout)
	}
	if !reflect.DeepEqual(p.Kinds, []string{"fact", "thought"}) {
		t.Errorf("Kinds = %+v, want [fact, thought]", p.Kinds)
	}
}

func TestParseFallback(t *testing.T) {
	p := Parse("fallback", []byte("# 纯正文，无 frontmatter"))
	if p.Target != "fallback" || p.Layout != "date" || len(p.Kinds) != 4 {
		t.Fatalf("回退默认策略不对: %+v", p)
	}
}
