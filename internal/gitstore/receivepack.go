package gitstore

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// 接收端：把别人推来的 pack 落进本仓库，并按 ref 更新分支。
//
// 为什么必须有它：注入一批已经成形的历史（另一台机器的备份、一次批量导入、
// 一段别人整理好的记录）时，逐条 put 会把一次导入拆成几千个切片，
// 时间与作者全丢。推一个分支才是它本来的样子。
//
// 三条硬规则，都不可让步：
//  1. 只接受快进。覆盖远端历史等于抹掉别人已经写下的东西，
//     那是 reset 的职责（要显式做、要 root），不是 push 的默认行为。
//  2. 只接受 refs/heads/*。tag、notes 之类要改的是别的语义，
//     从这条通道进来会绕过各自的校验。
//  3. 不允许删除分支（new 全零）。删除是「需要人负责」的动作。
//
// 对象落地与 ref 更新是分开的两步：先收整个 pack 并确认对象都在，
// 再逐个 ref 提交。中途失败时对象多出来但 ref 没动——那是对的形态，
// 悬空对象等 gc，脏 ref 才是事故。

const zeroOID = "0000000000000000000000000000000000000000"

// receiveCommand 是一条 ref 更新请求。
type receiveCommand struct {
	Old string
	New string
	Ref string
}

// ReceivePack 处理一次 git-receive-pack 请求体，返回 report-status 响应。
//
// allowRef 决定哪些 ref 允许写——权限判定留在调用方（服务端知道凭证），
// 这一层只懂仓库。
func (r *Repo) ReceivePack(body io.Reader, allowRef func(ref string) bool) ([]byte, error) {
	cmds, err := readReceiveCommands(body)
	if err != nil {
		return nil, err
	}
	if len(cmds) == 0 {
		return nil, errors.New("客户端没有提交任何 ref 更新")
	}

	// 剩下的字节就是 pack。
	var pack []byte
	if body != nil {
		pack, err = io.ReadAll(io.LimitReader(body, maxIngestBytes+1))
		if err != nil {
			return nil, fmt.Errorf("读取 pack 失败: %w", err)
		}
		if len(pack) > maxIngestBytes {
			return nil, fmt.Errorf("pack 超过 %d 字节上限", maxIngestBytes)
		}
	}

	report := func(unpack string, perRef []string) []byte {
		var out []byte
		out = append(out, PktLine("unpack "+unpack+"\n")...)
		for _, line := range perRef {
			out = append(out, PktLine(line+"\n")...)
		}
		out = append(out, PktFlush()...)
		return out
	}

	if len(pack) > 0 {
		if _, err := r.IngestPack(pack); err != nil {
			return report(fmt.Sprintf("fail %s", sanitizeReason(err.Error())), nil), nil
		}
	}

	var results []string
	applied := 0
	for _, cmd := range cmds {
		reason := r.applyCommand(cmd, allowRef)
		if reason == "" {
			results = append(results, "ok "+cmd.Ref)
			applied++
			continue
		}
		results = append(results, fmt.Sprintf("ng %s %s", cmd.Ref, sanitizeReason(reason)))
	}
	if applied == 0 {
		return report("aborted 没有 ref 被更新", results), nil
	}
	return report("ok", results), nil
}

// applyCommand 落一条 ref 更新，返回拒绝理由（空串表示成功）。
func (r *Repo) applyCommand(cmd receiveCommand, allowRef func(ref string) bool) string {
	if !strings.HasPrefix(cmd.Ref, "refs/heads/") {
		return "只接受 refs/heads/* 的推送"
	}
	if cmd.New == zeroOID {
		return "不允许通过 push 删除分支"
	}
	if validSHA(cmd.New) != nil {
		return "新的提交号不合法"
	}
	if allowRef != nil && !allowRef(cmd.Ref) {
		return "这张凭证没有该分支的写权限"
	}
	if !r.HasObject(cmd.New) {
		return "pack 里没有这个提交对象"
	}
	if cmd.Old != zeroOID {
		if !r.HasObject(cmd.Old) {
			return "本仓库缺少远端记录的旧提交，无法判断快进"
		}
		ok, err := r.IsAncestor(cmd.Old, cmd.New)
		if err != nil {
			return "无法判断快进关系：" + err.Error()
		}
		if !ok {
			return "非快进：这会改写已有历史（要覆盖请用 reset）"
		}
	}
	if err := r.SetRef(cmd.Ref, cmd.New); err != nil {
		return "写入 ref 失败：" + err.Error()
	}
	return ""
}

// readReceiveCommands 读出 flush 之前的全部 ref 更新请求。
func readReceiveCommands(body io.Reader) ([]receiveCommand, error) {
	var cmds []receiveCommand
	first := true
	for {
		line, err := ReadPktLine(body)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}
		if line == "" {
			break
		}
		if first {
			first = false
			if i := strings.IndexByte(line, 0); i >= 0 {
				line = line[:i]
			}
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if validSHA(fields[0]) != nil && fields[0] != zeroOID {
			continue
		}
		if validSHA(fields[1]) != nil && fields[1] != zeroOID {
			continue
		}
		cmds = append(cmds, receiveCommand{Old: fields[0], New: fields[1], Ref: fields[2]})
	}
	return cmds, nil
}

// sanitizeReason 把拒绝理由压成一行。
//
// report-status 的每条结果是一个 pkt-line，理由里带换行会把协议帧拆断，
// 客户端会报一个与真实原因无关的解析错误。
func sanitizeReason(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
