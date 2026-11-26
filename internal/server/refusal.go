package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// 拒绝的种类。这是**给调用方分支用的**，不是给人看的措辞。
//
// 为什么需要它（issue #12）：早先所有失败都是 JSON-RPC 的 -32603，
// 「地址不存在」「这张凭证看不到这个分支」「参数写错了」「对端连不上」
// 共用同一个 code，调用方只能去解析 message 文本——而措辞一改就静默失效。
// 模型调用方尤其受害：它分不清「查不到」与「查不了」，于是换个措辞再查一遍。
//
// 命名对齐 MCP / JSON-RPC 生态里已有的说法，不发明新词。
type RefusalKind string

const (
	// RefusalNotFound：要的东西不存在。带上被查地址、范围，让调用方能解释。
	RefusalNotFound RefusalKind = "not_found"
	// RefusalForbidden：东西可能存在，但这张凭证碰不到。
	RefusalForbidden RefusalKind = "forbidden"
	// RefusalInvalidArgument：请求本身写错了（语法、参数、缺必填项）。
	RefusalInvalidArgument RefusalKind = "invalid_argument"
	// RefusalUnavailable：依赖的远端/服务不可达或超时。
	RefusalUnavailable RefusalKind = "unavailable"
)

// Refusal 是一次带种类的拒绝。
//
// Detail 是给人读的正文（保持既有的措辞，help 与测试都依赖它）；
// Kind 与 Data 是给程序判的。两者都要，缺一个就有一类调用方遭殃。
type Refusal struct {
	Kind   RefusalKind
	Detail string
	// Data 是随拒绝一起给出的结构化事实，例如被查的地址、范围、项目。
	Data map[string]any
}

func (r *Refusal) Error() string { return r.Detail }

// Unwrap 让 errors.Is/As 在链上也认得出种类。
func (r *Refusal) Unwrap() error { return nil }

// refuse 构造一次拒绝。detail 允许为空（有些站点只给结构化事实）。
func refuse(kind RefusalKind, detail string, data map[string]any) *Refusal {
	return &Refusal{Kind: kind, Detail: detail, Data: data}
}

// refusef 是带格式化的 refused。
func refusef(kind RefusalKind, data map[string]any, format string, args ...any) *Refusal {
	return &Refusal{Kind: kind, Detail: fmt.Sprintf(format, args...), Data: data}
}

// notFoundf / forbiddenf / invalidf / unavailablef 是各站点的主要入口。
func notFoundf(data map[string]any, format string, args ...any) *Refusal {
	return refusef(RefusalNotFound, data, format, args...)
}

func forbiddenf(data map[string]any, format string, args ...any) *Refusal {
	return refusef(RefusalForbidden, data, format, args...)
}

func invalidf(data map[string]any, format string, args ...any) *Refusal {
	return refusef(RefusalInvalidArgument, data, format, args...)
}

func unavailablef(data map[string]any, format string, args ...any) *Refusal {
	return refusef(RefusalUnavailable, data, format, args...)
}

// mcpCodeOf 把种类映射成 JSON-RPC 数值码。
//
// 取值规则：
//   - -32602 是 JSON-RPC 标准里的 invalid params，参数错用它，别自造；
//   - -32000..-32099 是标准留给实现自定义的区间，not_found / forbidden /
//     unavailable 各占一个；
//   - 其余仍是 -32603（internal），**不被猜测**：不在上面几类里的失败，
//     我们确实不知道原因，就不假装知道。
func mcpCodeOf(kind RefusalKind) int {
	switch kind {
	case RefusalInvalidArgument:
		return -32602
	case RefusalNotFound:
		return -32004
	case RefusalForbidden:
		return -32003
	case RefusalUnavailable:
		return -32001
	default:
		return -32603
	}
}

// asRefusal 把任意 error 归一成 *Refusal（认不出就返回 nil）。
//
// 只认**显式构造**的拒绝，不做文案匹配。
//
// 早先这里想过用 message 前缀去猜种类（"not found" → not_found 之类），
// 那是把 issue #12 抱怨的那个毛病换个地方重犯：措辞一改，分类静默错位，
// 而错位的分类比没有分类更糟——调用方会以为它拿到的是确定答案。
// 因此没标种类的失败一律保持 -32603，诚实地说「这是内部错误」。
func asRefusal(err error) *Refusal {
	if err == nil {
		return nil
	}
	var r *Refusal
	if errors.As(err, &r) {
		return r
	}
	return nil
}

// wrapTransport 把网络类失败标成 unavailable。
//
// 只做一件事：给已知的传输失败补上种类，其余原样返回。
// 判断依据是错误链（context deadline、net.Error），不是文案。
func wrapTransport(err error, what string) error {
	if err == nil {
		return nil
	}
	if r := asRefusal(err); r != nil {
		return err
	}
	var nerr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		(errors.As(err, &nerr) && nerr.Timeout()) {
		return unavailablef(map[string]any{"target": what}, "%s: %v", what, err)
	}
	return err
}

// refusalMeta 给出结果里 _meta 需要的机器可判字段。
//
// 为什么放 _meta 而不是 structuredContent：MCP 的 structuredContent 是
// 按需的第二投影（默认关着，理由是不让每次回复收两份钱），而「这次是不是
// 什么都没查到」是**协议层事实**，调用方不该为它先付一份投影的钱。
// _meta 就是 MCP 留给这种事实的位置，量级几十字节。
func refusalMeta(empty bool, code string) map[string]any {
	m := map[string]any{"memcp/found": !empty}
	if empty && code != "" {
		m["memcp/reason"] = code
	}
	return m
}

// metaFor 由 TermResult 生成 _meta。
func metaFor(r TermResult) map[string]any {
	if r.Empty {
		reason := r.EmptyReason
		if reason == "" {
			reason = string(RefusalNotFound)
		}
		return refusalMeta(true, reason)
	}
	return refusalMeta(false, "")
}

// refusalReasonOf 把「什么都没查到」的原因压成一个词，供 _meta 使用。
func refusalReasonOf(kind RefusalKind) string {
	return strings.TrimSpace(string(kind))
}
