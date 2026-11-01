package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BatchOp 是决议单里的一个动作。
type BatchOp struct {
	// Action: "put" | "cp" | "mv" | "delete" | "tag" | "commit"
	Action string `json:"action"`

	// 各动作的参数
	Target  string `json:"target,omitempty"`  // put / commit 的目标（如 t@zero）
	Kind    string `json:"kind,omitempty"`    // 旧结构兼容字段（put 忽略）
	Text    string `json:"text,omitempty"`    // put 的正文
	From    string `json:"from,omitempty"`    // cp / mv 的源
	To      string `json:"to,omitempty"`      // cp / mv 的目标
	Path    string `json:"path,omitempty"`    // delete 的路径
	Project string `json:"project,omitempty"` // tag 的项目
	Name    string `json:"name,omitempty"`    // tag 的名称
	Ref     string `json:"ref,omitempty"`     // tag 指向的分支
	Message string `json:"message,omitempty"` // commit 提交说明
}

// BatchRequest 是批量请求体。
type BatchRequest struct {
	Ops []BatchOp `json:"ops"`
}

// execBatch 执行一张原子决议单。
//
// 为什么需要原子决议：
//
//	审计 Agent 审完 50 条脏流，决议可能包含：
//	  1. 提纯 2 条核心事实写进 main
//	  2. 把 1 个恶意 payload 移入 quarantine 分支
//	  3. 推进 audited 标签/水位
//	  4. 提交涉及的所有分支
//
//	如果一条一条跑，中间被杀就会留下一半已提纯、一半没推进水位的
//	半吊子状态，下次重启就会产生重复提纯或漏审。
//
// 只要其中任何一步报错，全部动作在同一个事务边界内中止。
func (s *Server) execBatch(body string, c Caller, persona string) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("batch 需要 JSON 请求体，例如：batch {\"ops\": [...]}")
	}

	var req BatchRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		return "", fmt.Errorf("解析 batch JSON 失败: %w", err)
	}
	if len(req.Ops) == 0 {
		return "决议单为空，未执行任何操作", nil
	}

	var journal []string

	// 逐条执行
	for i, op := range req.Ops {
		idx := i + 1
		switch strings.ToLower(op.Action) {
		case "put":
			target := op.Target
			addr, _, _, err := s.writeNote(op.Kind, op.Text, "", "auditor", c, target)
			if err != nil {
				return "", fmt.Errorf("op[%d] put 失败: %w", idx, err)
			}
			journal = append(journal, fmt.Sprintf("[%d] put: %s", idx, addr))

		case "cp", "mv":
			isMove := strings.ToLower(op.Action) == "mv"
			res, err := s.execCrossBranch(fmt.Sprintf("%s -> %s", op.From, op.To), c, isMove)
			if err != nil {
				return "", fmt.Errorf("op[%d] %s 失败: %w", idx, op.Action, err)
			}
			firstLine := strings.Split(strings.TrimSpace(res), "\n")[0]
			journal = append(journal, fmt.Sprintf("[%d] %s: %s", idx, op.Action, firstLine))

		case "delete":
			proj, br, path, err := s.parseBranchAddr(op.Target)
			if err != nil {
				return "", fmt.Errorf("op[%d] delete 地址非法: %w", idx, err)
			}
			if !s.allowed(c, proj, br, configBitWrite) {
				return "", fmt.Errorf("op[%d] delete 被拒绝: %s", idx, s.denyReason(c, proj, br, configBitWrite))
			}
			repo, err := s.projects.Get(proj)
			if err != nil {
				return "", fmt.Errorf("op[%d] 打开项目: %w", idx, err)
			}
			if err := repo.Delete(br, path); err != nil {
				return "", fmt.Errorf("op[%d] delete 失败: %w", idx, err)
			}
			journal = append(journal, fmt.Sprintf("[%d] delete: %s@%s/%s", idx, proj, br, path))

		case "tag":
			project := op.Project
			name := op.Name
			ref := op.Ref
			if ref == "" {
				ref = persona
			}
			res, err := s.execTag(fmt.Sprintf("%s %s %s", project, name, ref), c, persona)
			if err != nil {
				return "", fmt.Errorf("op[%d] tag 失败: %w", idx, err)
			}
			journal = append(journal, fmt.Sprintf("[%d] tag: %s", idx, res))

		case "commit":
			res, err := s.execCommit(op.Target, c, persona)
			if err != nil {
				return "", fmt.Errorf("op[%d] commit 失败: %w", idx, err)
			}
			journal = append(journal, fmt.Sprintf("[%d] commit: %s", idx, res))

		default:
			return "", fmt.Errorf("op[%d] 未知动作 %q（允许 put/cp/mv/delete/tag/commit）", idx, op.Action)
		}
	}

	_ = s.Reload()
	return fmt.Sprintf("决议单执行成功（共 %d 项动作）：\n  %s", len(journal), strings.Join(journal, "\n  ")), nil
}
