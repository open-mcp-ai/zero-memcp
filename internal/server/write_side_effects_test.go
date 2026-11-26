package server

import (
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 每一个会改树内容的动词，都必须同时完成两件事：
//
//  1. 让同步层看得见这次变化（要么推进命令流位点，要么推进了 commit 基准树）
//     ——不登记，对端同步永远看不到；
//  2. 广播变更事件（notifications/resources/updated）——不广播，
//     订阅这棵树的分身或下游节点永远收不到通知。
//
// 这条表驱动测试存在的原因（2026-11-26）：两件事早先由每个写站点
// 自己记得调，结果连续四处各漏了一样（write 漏了 noteWriteOp 后又漏了
// 广播；apply / delete / cp / mv 从来没有广播；ref 从来不记命令流）。
// 现在收口进 note*Op，用这一张表把所有写动词一次钉死：以后新加的动词
// 只要漏一个，这里当场红。
func TestAllWriteVerbsRecordOpAndPublish(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Cap: "rwx"})

	// 订阅 blackegg/me@zero：这个测试用同一个分支走所有写动词。
	sub := srv.resources.add(Caller{User: "watcher", Identity: "watcher"}, []string{"memcp://blackegg%2Fme/zero"})
	defer srv.resources.remove(sub)

	drainEvent := func(name string) {
		t.Helper()
		select {
		case ev := <-sub.ch:
			if !strings.HasPrefix(ev.URI, "memcp://blackegg%2Fme/zero") {
				t.Fatalf("%s: 收到意外 URI %s", name, ev.URI)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: 写了树却没有广播事件——订阅了这棵树的分身收不到通知", name)
		}
	}

	opPos := func() (int64, string) {
		l, err := srv.opLogs.For("blackegg/me", "zero")
		if err != nil {
			t.Fatalf("打开 oplog 失败: %v", err)
		}
		return l.Last(), l.BaseTree()
	}

	run := func(name string, call map[string]any) string {
		t.Helper()
		bSeq, bBase := opPos()
		out, err := callMCPRaw(t, srv, tok, call)
		if err != nil {
			t.Fatalf("%s 失败: %v", name, err)
		}
		aSeq, aBase := opPos()
		// 同步可见只有两条合法路：在原基准上推进位点，或者折叠 commit
		// 推进基准树（mv 就是这么做的）。两条都没动 = 同步完全看不见这次写。
		if aSeq <= bSeq && aBase == bBase {
			t.Fatalf("%s 写完却既没推进命令流位点也没推进基准树（seq %d->%d, base %s->%s）——同步会凭空丢改动",
				name, bSeq, aSeq, shortSHA(bBase), shortSHA(aBase))
		}
		drainEvent(name)
		// 把同一动词多余的事件清干净，别串到下一个用例里。
		for len(sub.ch) > 0 {
			<-sub.ch
		}
		return out
	}

	// 1. put
	putOut := run("put", map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "put blackegg/me@zero $BODY",
			"vars": map[string]string{"BODY": "put 写的内容"},
		},
	})
	// 提取 put 返回的相对路径，后面 ref 用。
	putAddr := strings.TrimPrefix(strings.Fields(putOut)[1], "blackegg/me/")

	// 2. ref（今天刚发现它之前从来不记命令流）
	run("ref", map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd": "ref blackegg/me@zero blackegg/me@zero/" + putAddr,
		},
	})

	// 3. write
	run("write", map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero/direct.md $BODY",
			"vars": map[string]string{"BODY": "write 写的内容"},
		},
	})

	// 4. apply (write)
	run("apply-write", map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "apply blackegg/me@zero -- $P",
			"vars": map[string]string{"P": `[{"op":"write","path":"from-apply.md","content":"apply 写的"}]`},
		},
	})

	// 5. cp
	run("cp", map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd": "cp blackegg/me@zero/direct.md -> blackegg/me@zero/copied.md",
		},
	})

	// 6. mv（要求源路径存在，先写一个专给它搬的）
	run("write-for-mv", map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "write blackegg/me@zero/to-move.md $BODY",
			"vars": map[string]string{"BODY": "待搬运"},
		},
	})
	run("mv", map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd": "mv blackegg/me@zero/to-move.md -> blackegg/me@zero/moved.md",
		},
	})

	// 7. apply (delete)
	run("apply-delete", map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd":  "apply blackegg/me@zero -- $P",
			"vars": map[string]string{"P": `[{"op":"delete","path":"from-apply.md"}]`},
		},
	})

	// 8. delete
	run("delete", map[string]any{
		"name": "term",
		"arguments": map[string]any{
			"cmd": "delete blackegg/me@zero/copied.md",
		},
	})
}
