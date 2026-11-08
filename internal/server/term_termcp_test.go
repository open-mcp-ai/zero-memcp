package server

import (
	"strings"
	"testing"
)

// TestTermcpSequentialAndGating 锁定基于 termcp/term 的 && / || / ; 调度。
func TestTermcpSequentialAndGating(t *testing.T) {
	srv, dataDir := newTestServer(t, "")
	c := srv.rootCaller()
	v := Viewer{Channel: "test"}

	// 1. 无条件连续执行 (;)
	res, err := srv.execTerm("status; help recall", v, c)
	if err != nil {
		t.Fatalf("连续执行报错: %v", err)
	}
	// 多 slot 应渲染为 TERM/1 信封
	if !strings.Contains(res.Text, "TERM/1 200 OK") {
		t.Fatalf("多 slot 回复应包含 TERM/1 信封，实际:\n%s", res.Text)
	}

	// 2. || 短路回退：第一句查不到（Empty/404）应触发第二句
	// 往 daily 写入一条明确切片
	if _, err := srv.execTermVars("put daily@zero:fact $1", nil, []string{"x"}, v, c); err != nil {
		// put 正文走 vars/args，直接测 recall
	}

	res, err = srv.execTerm("recall absolutely_no_matches_xyz || status", v, c)
	if err != nil {
		t.Fatalf("短路回退报错: %v", err)
	}
	if !strings.Contains(res.Text, "TERM/1 200 OK") || !strings.Contains(res.Text, "Verb: status") {
		t.Fatalf("第一句未命中时，|| 后的 status 必须被执行:\n%s", res.Text)
	}

	// 3. && 门控：第一句未命中（404）时，&& 后的语句必须被 208 跳过
	res, err = srv.execTerm("recall absolutely_no_matches_xyz && status", v, c)
	if err != nil {
		t.Fatalf("门控报错: %v", err)
	}
	if !strings.Contains(res.Text, "208 Already Reported") {
		t.Fatalf("第一句未命中时，&& 后的语句必须产生 208 跳过标记，实际:\n%s", res.Text)
	}

	_ = dataDir
}

// TestTermcpGrammarRejectsUnknownFlags 锁定词表的语法严格性。
func TestTermcpGrammarRejectsUnknownFlags(t *testing.T) {
	srv, _ := newTestServer(t, "")
	c := srv.rootCaller()
	v := Viewer{Channel: "test"}

	// 未声明的 flag 必须在 Parse 阶段就拒绝，不跑任何副作用
	_, err := srv.execTerm("status --evil-flag", v, c)
	if err == nil {
		t.Fatal("未声明的 flag 应报错")
	}
	if !strings.Contains(err.Error(), "unknown flag for status") {
		t.Fatalf("错误提示应指出未知的 flag，实际: %v", err)
	}
}

// TestTermcpPutRejectsMixedReference 锁定注入防护：引用不能与文字混在同一个词里。
func TestTermcpPutRejectsMixedReference(t *testing.T) {
	srv, _ := newTestServer(t, "")
	c := srv.rootCaller()
	v := Viewer{Channel: "test"}

	if _, err := srv.execTermVars("put daily@zero prefix$TEXT", map[string]string{"TEXT": "x"}, nil, v, c); err == nil {
		t.Fatal("引用与文字混在一个词里必须被拒绝")
	}
}

// TestTermcpPutWithTreeAndSession 锁定双位置参数的 put。
//
// 这是真实扩展每轮落盘用的形状：`put <target>/ @<session> -- $TEXT`。
// 早期的词表迁移在这里炸了：termcp 的 payload 动词只认一个 target，
// `@<session>` 和正文引用挤在同一个 payload span 里被拒绝
// （"a variable cannot sit inside a shell line"）。
// Go 测试先前只覆盖了「只有树」或「只有 @session」的形状，两者都有
// 的这条只在真实 pi 运行里才暴露——这也是「必须真跑」的又一个例证。
func TestTermcpPutWithTreeAndSession(t *testing.T) {
	srv, _ := newTestServer(t, "")
	c := srv.rootCaller()
	v := Viewer{Channel: "test"}

	res, err := srv.execTermVars(
		"put daily@zero:fact/ --session real-session-uuid $TEXT",
		map[string]string{"TEXT": "双位置参数的正文; 含分号 && 逻辑符"},
		nil, v, c)
	if err != nil {
		t.Fatalf("put <tree> @<session> -- $TEXT 应可解析并执行: %v", err)
	}
	if !strings.Contains(res.Text, "ok daily/") {
		t.Fatalf("应写入成功: %s", res.Text)
	}

	// @session 进的是切片 front-matter（session: xxx），不在 ok 行里。
	// 早先断言放在 ok 行上，永远不会命中——这是断言位置错，不是功能错。
	addr := strings.Fields(strings.TrimSpace(res.Text))
	var slice string
	for _, f := range addr {
		if strings.HasPrefix(f, "daily/") {
			slice = f
			break
		}
	}
	if slice == "" {
		t.Fatalf("无法从回复提取切片地址: %s", res.Text)
	}
	read, err := srv.execTermVars("read "+slice, nil, nil, v, c)
	if err != nil {
		t.Fatalf("read 回写切片失败: %v", err)
	}
	if !strings.Contains(read.Text, "session: real-session-uuid") {
		t.Fatalf("@session 应出现在 front-matter: %s", read.Text)
	}
	if !strings.Contains(read.Text, "双位置参数的正文; 含分号 && 逻辑符") {
		t.Fatalf("正文应逐字保留: %s", read.Text)
	}
}

// 引用校验的边界：写入路径必须 fail closed，字面量搜索必须放行。
//
// 这两侧都不能松：一边是把拼错的变量名当正文存进记忆库，另一边是把
// 「搜索字面量 $HOME」这个真实能力削掉。全局严格做不到，只有按动词分。
func TestReferenceCheckOnlyOnPayloadVerbs(t *testing.T) {
	srv, _ := newTestServer(t, "")
	c := srv.rootCaller()

	// 非 payload 动词：$ 归 termcp 管，这里放行（搜索字面量）。
	for _, line := range []string{"grep $HOME", "recall $PRICE", "log $5"} {
		if _, err := srv.execTermVars(line, map[string]string{}, nil, Viewer{}, c); err != nil {
			if strings.Contains(err.Error(), "is not declared in vars") {
				t.Errorf("%q 被当成了变量引用，字面量搜索被误拒: %v", line, err)
			}
		}
	}

	// payload 动词：未声明的引用必须拒绝，否则字面量进了记忆库。
	for _, line := range []string{
		"put daily@zero:fact $MISSING",
		"put --session s $MISSING",
		"write daily@zero:fact/x.md $MISSING",
		"remote-key put --name k $MISSING",
	} {
		_, err := srv.execTermVars(line, map[string]string{}, nil, Viewer{}, c)
		if err == nil || !strings.Contains(err.Error(), "is not declared in vars") {
			t.Errorf("%q 应该被拒绝（未声明引用），实际: %v", line, err)
		}
	}

	// 声明了就正常写入。
	if _, err := srv.execTermVars("put daily@zero:fact $TEXT", map[string]string{"TEXT": "正文"}, nil, Viewer{}, c); err != nil {
		t.Errorf("声明过的引用应该能写入: %v", err)
	}
}
