package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/memory"
)

// 对外写动作的 provenance 账本（issue #11）。
//
// # 它解决的两个具体问题
//
//  1. **事后无法自证**：外部平台上出现一条内容，要回答「哪个会话、什么时候、
//     用哪个身份、在哪个渠道、依据哪条记忆发出去的」——记忆里可能只写了
//     「我发过 X」，没有 actor / channel / 时间 / 外部 id，无法与外部系统的
//     记录对上，也无法自证不是别人发的。
//  2. **无法防重复执行**：同一动作被两个会话各做一次（或同一会话重试），
//     外部平台就多一条重复内容。没有任何地方能「按外部 id 查一下做过没有」。
//
// # 为什么不另起一套存储
//
// 账本条目就是 mem 里的一种内容：落在 <tree>/ledger/ 前缀下的普通条目。
// 于是 Git 历史、commit 折叠、recall / grep、权限模型、三节点同步
// 全部自动继承——第二份真相会漂移，第一份不会。
//
// # at 一律用服务端时间
//
// 客户端自报时间等于让「事件何时发生」变成调用方的意见。重试、时钟漂移、
// 恶意回填都会让它失真，而这条记录存在的意义正是「对得上外部系统的记录」。

// ledgerPrefix 是账本在树里的约定前缀。
//
// 用固定前缀而不是单独的仓库：账本与它所描述的记忆同属一个世界的同一份历史，
// 分行存储会让「这条回执对应哪条记忆」在同步与备份上都多出一层可错之处。
const ledgerPrefix = "ledger"

// ledgerRecord 是一条对外写动作的回执。
type ledgerRecord struct {
	ExternalID  string `json:"external_id"`
	Actor       string `json:"actor"`
	Identity    string `json:"identity"`
	Channel     string `json:"channel"`
	Action      string `json:"action"`
	Result      string `json:"result"`
	ContentHash string `json:"content_hash,omitempty"`
	At          string `json:"at"`
	Addr        string `json:"addr"`
}

// execLedger 实现账本动词。
//
//	ledger check <external_id>                     查重：这个 external_id 做过没有
//	ledger put <external_id> --result R [--identity I] [--channel C] [--action A]
//	                                        [--hash H] [--tree <tree>]
//	ledger list [--identity I] [--since T] [--limit N]
//
// 查重的答案必须是**明确的二元**：有记录 / 无记录。静默返回空、返回 null、
// 或把「没做过」当错误抛出，都会让调用方在两件事之间猜——而这两个猜测的
// 后果完全相反（重复发一条 vs 该发没发）。
// splitLedgerArgs 把 `rest` 拆成位置词与剩余 flag 串。
//
// 必须这样做，因为 termcp 重建命令行时会把 `--result ok --channel forum`
// 挪到 `st.Args` 前面（只有第一个词 stays in st.Target）：位置参数与
// flag 的相对顺序会被重排。按 flag-value 对把位置词提取出来，无论顺序
// 怎么重排都能准确拿到 subcommand 与 external_id。
func splitLedgerArgs(rest string) (pos []string, flagsStr string) {
	fields := strings.Fields(rest)
	var flags []string
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if strings.HasPrefix(f, "--") {
			flags = append(flags, f)
			if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
				flags = append(flags, fields[i+1])
				i++
			}
			continue
		}
		pos = append(pos, f)
	}
	return pos, strings.Join(flags, " ")
}

func (s *Server) execLedger(rest string, c Caller) (TermResult, error) {
	pos, flagsStr := splitLedgerArgs(rest)
	if len(pos) == 0 {
		return TermResult{}, invalidf(nil, "ledger syntax: ledger check <external_id> | ledger put <external_id> --result <ok|failed:...> [flags] | ledger list [--identity I] [--since T] [--limit N]")
	}
	switch pos[0] {
	case "check":
		if len(pos) < 2 {
			return TermResult{}, invalidf(nil, "ledger check syntax: ledger check <external_id>")
		}
		return s.ledgerCheck(pos[1], c)
	case "put":
		if len(pos) < 2 {
			return TermResult{}, invalidf(nil, "ledger put syntax: ledger put <external_id> --result <ok|failed:...> [--identity I] [--channel C] [--action A] [--hash H] [--tree <tree>]")
		}
		return s.ledgerPut(pos[1], flagsStr, c)
	case "list":
		return s.ledgerList(flagsStr, c)
	default:
		return TermResult{}, invalidf(nil, "ledger: unknown subcommand %q (supported: check, put, list)", pos[0])
	}
}

// ledgerPut 记录一条回执。同一 (external_id, result) 重复登记是幂等的：
// 返回已有那条记录，而不是悄悄追加第二条自相矛盾的记录。
func (s *Server) ledgerPut(externalID, rest string, c Caller) (TermResult, error) {
	externalID = strings.TrimSpace(externalID)
	if externalID == "" || strings.ContainsAny(externalID, " \t\n") {
		return TermResult{}, invalidf(nil, "ledger put: external_id must be one non-empty token")
	}
	vals, bad := parseLedgerFlags(rest, map[string]bool{
		"--result": true, "--identity": true, "--channel": true,
		"--action": true, "--hash": true, "--tree": true,
	})
	if bad != "" {
		return TermResult{}, invalidf(nil, "ledger put: unknown flag %q (supported: --result, --identity, --channel, --action, --hash, --tree)", bad)
	}
	flagVal := func(name string) string { return vals[name] }
	result := flagVal("--result")
	if result == "" {
		// 失败也要写：不写 result 的回执等于没写，重试时查重会误判成「没做过」。
		return TermResult{}, invalidf(nil, "ledger put: --result is required (ok, failed:<reason>, or unknown)")
	}
	tree := flagVal("--tree")

	// actor 是发起动作的会话/进程标识，取自凭证身份；identity 是对外身份标签。
	// identity 只存可读标识，**从不存凭证**——账本不是密钥库。
	actor := c.Identity
	if actor == "" {
		actor = "mcp"
	}
	identity := flagVal("--identity")
	if identity == "" {
		identity = actor
	}
	channel := flagVal("--channel")
	action := flagVal("--action")
	hash := flagVal("--hash")
	if hash == "" {
		sum := sha256.Sum256([]byte(externalID + "\x00" + result))
		hash = hex.EncodeToString(sum[:])[:12]
	}

	// 幂等：同一个 external_id 的同一个 result 已经有记录，就返回它。
	//
	// 同一个 external_id 的**不同** result（failed → ok）会作为新记录追加，
	// 历史保留：重试成功这件事本身就是要审计的事实。
	existing, err := s.ledgerLookup(externalID, tree, c)
	if err != nil {
		return TermResult{}, err
	}
	for _, r := range existing {
		if r.Result == result {
			return TermResult{
				Text: fmt.Sprintf("already recorded %s at %s (result=%s)\n  %s",
					externalID, r.At, r.Result, r.Addr),
				Structured: map[string]any{
					"found":       true,
					"external_id": externalID,
					"existing":    true,
					"record":      r,
					"records":     existing,
				},
			}, nil
		}
	}

	// 服务端时间是唯一时间来源（见文件顶部注释）。
	now := time.Now()
	body := fmt.Sprintf("external_id: %s\nat: %s\nactor: %s\nidentity: %s\nchannel: %s\naction: %s\nresult: %s\ncontent_hash: %s\n",
		externalID, now.Format(time.RFC3339), actor, identity, channel, action, result, hash)

	treePrefix := ledgerPrefix
	if tree != "" {
		treePrefix = strings.Trim(tree, "/") + "/" + ledgerPrefix
	}
	addr, proj, branch, _, err := s.writeNoteGit("", body, "", c.Identity, c, tree, treePrefix)
	if err != nil {
		return TermResult{}, err
	}
	rel := strings.TrimPrefix(addr, proj+"/")
	rec := ledgerRecord{
		ExternalID: externalID, Actor: actor, Identity: identity, Channel: channel,
		Action: action, Result: result, ContentHash: hash,
		At: now.Format(time.RFC3339), Addr: proj + "@" + branch + ":" + rel,
	}
	return TermResult{
		Text: fmt.Sprintf("recorded %s (%s) at %s\n  %s", externalID, result, rec.At, rec.Addr),
		Structured: map[string]any{
			"found":       true,
			"external_id": externalID,
			"existing":    false,
			"record":      rec,
			"addr":        rec.Addr,
		},
	}, nil
}

// ledgerCheck 按 external_id 查重。
func (s *Server) ledgerCheck(externalID string, c Caller) (TermResult, error) {
	externalID = strings.TrimSpace(externalID)
	recs, err := s.ledgerLookup(externalID, "", c)
	if err != nil {
		return TermResult{}, err
	}
	if len(recs) == 0 {
		// 「无记录」是答案，不是错误：调用方要据此决定「去执行」。
		// 按 issue #12 的口径，未命中走 Empty + reason，而不是 isError。
		return TermResult{
			Text:        fmt.Sprintf("no record for external_id %s", externalID),
			Empty:       true,
			EmptyReason: string(RefusalNotFound),
			Structured: map[string]any{
				"found":       false,
				"external_id": externalID,
				"records":     []ledgerRecord{},
			},
		}, nil
	}
	var lines []string
	for _, r := range recs {
		lines = append(lines, fmt.Sprintf("%s  at=%s actor=%s identity=%s channel=%s action=%s result=%s hash=%s",
			r.Addr, r.At, r.Actor, r.Identity, r.Channel, r.Action, r.Result, r.ContentHash))
	}
	return TermResult{
		Text: strings.Join(lines, "\n"),
		Structured: map[string]any{
			"found":       true,
			"external_id": externalID,
			"records":     recs,
			"latest":      recs[0],
		},
	}, nil
}

// ledgerList 按身份与时间区间审计。
func (s *Server) ledgerList(rest string, c Caller) (TermResult, error) {
	vals, bad := parseLedgerFlags(rest, map[string]bool{
		"--identity": true, "--since": true, "--limit": true,
	})
	if bad != "" {
		return TermResult{}, invalidf(nil, "ledger list: unknown flag %q (supported: --identity, --since, --limit)", bad)
	}
	flagVal := func(name string) string { return vals[name] }
	identity := flagVal("--identity")
	since := flagVal("--since")
	limit := 50
	if v := flagVal("--limit"); v != "" {
		if n, err := parseIntPositive(v); err == nil {
			limit = n
		}
	}
	all, err := s.ledgerLookup("", "", c)
	if err != nil {
		return TermResult{}, err
	}
	var kept []ledgerRecord
	for _, r := range all {
		if identity != "" && r.Identity != identity {
			continue
		}
		if since != "" && r.At < since {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == 0 {
		return TermResult{
			Text:        "no ledger records match",
			Empty:       true,
			EmptyReason: string(RefusalNotFound),
			Structured:  map[string]any{"found": false, "records": []ledgerRecord{}},
		}, nil
	}
	if len(kept) > limit {
		kept = kept[:limit]
	}
	var lines []string
	for _, r := range kept {
		lines = append(lines, fmt.Sprintf("%s  external_id=%s at=%s actor=%s identity=%s channel=%s action=%s result=%s",
			r.Addr, r.ExternalID, r.At, r.Actor, r.Identity, r.Channel, r.Action, r.Result))
	}
	return TermResult{Text: strings.Join(lines, "\n"), Structured: map[string]any{"found": true, "records": kept}}, nil
}

// ledgerLookup 扫账本条目。externalID 为空时返回全部。
//
// 读的是与 grep 相同的候选集（索引里的块），所以权限判据与 read 完全一致：
// 看不见的分支不会因为换了个动词就漏出来。
func (s *Server) ledgerLookup(externalID, tree string, c Caller) ([]ledgerRecord, error) {
	prefix := ledgerPrefix + "/"
	if tree != "" {
		prefix = strings.Trim(tree, "/") + "/" + ledgerPrefix + "/"
	}
	var out []ledgerRecord
	for _, tr := range s.treeList() {
		if !s.allowed(c, tr.Project, tr.Branch, configBitRead) {
			continue
		}
		for _, b := range tr.Blocks {
			if b.Hidden || !strings.Contains("/"+b.Path+"/", "/"+prefix) {
				continue
			}
			data, rerr := s.readBlock(b)
			if rerr != nil {
				continue
			}
			rec, ok := parseLedgerRecord(string(data))
			if !ok {
				continue
			}
			if externalID != "" && rec.ExternalID != externalID {
				continue
			}
			virt := s.virtualPath(c, b.Project, b.Branch, b.Path)
			if virt == "" {
				continue
			}
			rec.Addr = virt
			out = append(out, rec)
		}
	}
	// 时间倒序：最新的回执在最前，查重看第一条就够。
	sort.SliceStable(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out, nil
}

// parseLedgerRecord 从条目正文里取出回执字段。
//
// 正文是 key: value 逐行——与 mem 里其它条目的形状一致，所以 grep / read
// 对账本同样好用（想按渠道筛就用 grep），不必另造查询语言。
func parseLedgerRecord(text string) (ledgerRecord, bool) {
	var rec ledgerRecord
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "external_id":
			rec.ExternalID = v
		case "at":
			rec.At = v
		case "actor":
			rec.Actor = v
		case "identity":
			rec.Identity = v
		case "channel":
			rec.Channel = v
		case "action":
			rec.Action = v
		case "result":
			rec.Result = v
		case "content_hash":
			rec.ContentHash = v
		}
	}
	return rec, rec.ExternalID != ""
}

func parseIntPositive(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(ch-'0')
	}
	if n <= 0 {
		return 0, fmt.Errorf("not positive")
	}
	return n, nil
}

// ledgerAddr 返回一条账本条目的块 ID（供测试与审计使用）。
func ledgerAddr(project, branch, path string) string {
	return memory.MakeID(project, branch, path)
}

func parseLedgerFlags(rest string, allowed map[string]bool) (vals map[string]string, unknown string) {
	vals = make(map[string]string)
	fs := strings.Fields(rest)
	for i := 0; i < len(fs); i++ {
		f := fs[i]
		if !strings.HasPrefix(f, "-") {
			continue
		}
		if !allowed[f] {
			return nil, f
		}
		if i+1 < len(fs) {
			vals[f] = fs[i+1]
			i++
		}
	}
	return vals, ""
}
