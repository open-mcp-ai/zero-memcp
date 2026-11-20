package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// write / delete 必须只动它自己那一个 path，不能重扫整条分支。
//
// 为什么要有这条测试：这两个动词早先调 refreshBranch —— 那条路会把整条
// 分支重扫一遍（List 全部路径、每个 .md 读两次、全部重新分词）。
// 实测在 main.chat（5233 个文件）上一次 write 要 19 秒、一次 delete 18 秒，
// 而它们真正改变的只有一个文件。规模越大越慢，正是「越用越卡」的来源。
//
// 判据不看绝对耗时（机器不同），看**随分支规模的增长方式**：
// 先在 12 个文件的库上量一次，再在 600 个文件的库上量一次。
// 真正的增量实现，两者应当同一量级；退回全分支重扫则会是几十倍。
func TestWriteAndDeleteDoNotScaleWithBranchSize(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}

	measure := func(t *testing.T, bulk int) (writeEl, deleteEl time.Duration) {
		t.Helper()
		srv, dataDir := setupMountServer(t)
		tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

		// 造一堆已有文件，让分支有规模。
		for i := 0; i < bulk; i++ {
			if _, err := callMCP(t, srv, tok,
				fmt.Sprintf("put blackegg/me@fact -- filler-%d body-%d", i, i)); err != nil {
				t.Fatalf("seed put %d: %v", i, err)
			}
		}
		if _, err := callMCP(t, srv, tok, "commit blackegg/me@fact -m seed"); err != nil {
			t.Fatalf("seed commit: %v", err)
		}

		// 量 write。
		t0 := time.Now()
		if _, err := callMCP(t, srv, tok, "write blackegg/me@fact:probe-w.md -- hello"); err != nil {
			t.Fatalf("write: %v", err)
		}
		writeEl = time.Since(t0)

		// 量 delete。
		t0 = time.Now()
		if _, err := callMCP(t, srv, tok, "delete blackegg/me@fact:probe-w.md"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		deleteEl = time.Since(t0)
		return writeEl, deleteEl
	}

	smallW, smallD := measure(t, 12)
	bigW, bigD := measure(t, 600)

	t.Logf("12 文件:  write=%v  delete=%v", smallW, smallD)
	t.Logf("600 文件: write=%v  delete=%v", bigW, bigD)

	// 50 倍的数据量，耗时不应超过 10 倍。全分支重扫会是几十倍。
	ratio := func(big, small time.Duration) float64 {
		if small <= 0 {
			small = time.Millisecond
		}
		return float64(big) / float64(small)
	}
	if r := ratio(bigW, smallW); r > 10 {
		t.Fatalf("write 耗时随分支规模增长 %.1fx（12→600 文件）；"+
			"它应当只动单个 path，不重扫整条分支", r)
	}
	if r := ratio(bigD, smallD); r > 10 {
		t.Fatalf("delete 耗时随分支规模增长 %.1fx（12→600 文件）；"+
			"它应当只摘掉那一个 doc，不重扫整条分支", r)
	}
}

// 增量刷新不能把「新写进去的东西搜不到」或「删掉的东西还搜得到」当成功。
func TestRefreshPathKeepsIndexConsistent(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "test", Cap: "rwx"})

	// 新建一个文件，立刻可搜。
	if _, err := callMCP(t, srv, tok, "write blackegg/me@fact:toolbox.md -- marker-write-alpha"); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := callMCP(t, srv, tok, "recall marker-write-alpha --scope blackegg/me@fact")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(out, "marker-write-alpha") && !contains(out, "toolbox.md") {
		t.Fatalf("刚 write 的内容搜不到：%s", out)
	}

	// 覆盖同一个文件：旧内容不该还留在索引里。
	if _, err := callMCP(t, srv, tok, "write blackegg/me@fact:toolbox.md -- marker-write-beta"); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	out, err = callMCP(t, srv, tok, "grep marker-write-alpha --scope blackegg/me@fact")
	if err != nil {
		t.Fatal(err)
	}
	if contains(out, "marker-write-alpha") {
		t.Fatalf("覆盖写之后旧内容还在索引里：%s", out)
	}

	// 删除之后不该还搜得到。
	if _, err := callMCP(t, srv, tok, "delete blackegg/me@fact:toolbox.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	out, err = callMCP(t, srv, tok, "grep marker-write-beta --scope blackegg/me@fact")
	if err != nil {
		t.Fatal(err)
	}
	if contains(out, "marker-write-beta") {
		t.Fatalf("删除之后还能搜到：%s", out)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
