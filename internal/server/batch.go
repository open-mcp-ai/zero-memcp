package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BatchOp 是决议单里的一个动作。
// 字段名是 DSL 协议的一部分，JSON 示例与实现必须使用同一组字段；
// delete 使用 Target（完整地址），没有单独的 Path 字段。
type BatchOp struct {
	// Action: "put" | "cp" | "mv" | "delete" | "tag" | "commit"
	Action string `json:"action"`

	// 各动作的参数
	Target  string `json:"target,omitempty"`  // put / commit / delete 的目标（如 t@zero 或 t@zero/x.md）
	Kind    string `json:"kind,omitempty"`    // put 的 kind（如 fact / decision）
	Text    string `json:"text,omitempty"`    // put 的正文
	From    string `json:"from,omitempty"`    // cp / mv 的源
	To      string `json:"to,omitempty"`      // cp / mv 的目标
	Project string `json:"project,omitempty"` // tag 的项目
	Name    string `json:"name,omitempty"`    // tag 的名称
	Ref     string `json:"ref,omitempty"`     // tag 指向的分支
	Message string `json:"message,omitempty"` // 预留字段（当前 commit 动作不读取）
}

// BatchRequest 是批量请求体。
type BatchRequest struct {
	Ops []BatchOp `json:"ops"`
}

// execBatch 执行一张决议单。
//
// 为什么需要它：一次要搬运一批切片时，逐个执行会产生几十个中间提交，
// 历史被切碎，而且调用方要自己记「做到第几条了」。
//
// 它**不是**事务，也不假装是。遇到第一个错误就停，
// 已经执行的动作不回滚（脏层里已经落的写入不会撤回）。
// 早先这里的注释声称「要么全部成功，要么一个都不落」，
// 但代码里没有任何补偿逻辑——那是比没有保证更坏的东西，
// 因为调用方会按那句承诺去设计重试。
//
// 真正的用法是：把一批无依赖的写入打包（省中间提交），
// 失败时看错误里的 op 序号，从断点继续。需要真正的原子性，
// 就在单个分支上做一次 put + commit。
func (s *Server) execBatch(body string, c Caller, defaultBranch string) (string, error) {
	if err := s.requireRoot(c); err != nil {
		return "", err
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("batch requires a JSON request body, e.g. batch {\"ops\": [...]}")
	}

	var req BatchRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		return "", fmt.Errorf("parse batch JSON: %w", err)
	}
	if len(req.Ops) == 0 {
		return "operation list is empty; no operations were executed", nil
	}

	var journal []string

	// 逐条执行
	for i, op := range req.Ops {
		idx := i + 1
		switch strings.ToLower(op.Action) {
		case "put":
			target := op.Target
			addr, _, _, err := s.writeNote(op.Kind, op.Text, "", "batch", c, target)
			if err != nil {
				return "", fmt.Errorf("op[%d] put failed: %w", idx, err)
			}
			journal = append(journal, fmt.Sprintf("[%d] put: %s", idx, addr))

		case "cp", "mv":
			isMove := strings.ToLower(op.Action) == "mv"
			res, err := s.execCrossBranch(fmt.Sprintf("%s -> %s", op.From, op.To), c, isMove)
			if err != nil {
				return "", fmt.Errorf("op[%d] %s failed: %w", idx, op.Action, err)
			}
			firstLine := strings.Split(strings.TrimSpace(res), "\n")[0]
			journal = append(journal, fmt.Sprintf("[%d] %s: %s", idx, op.Action, firstLine))

		case "delete":
			proj, br, path, err := s.parseBranchAddr(op.Target)
			if err != nil {
				return "", fmt.Errorf("op[%d] invalid delete address: %w", idx, err)
			}
			if !s.allowed(c, proj, br, configBitWrite) {
				return "", fmt.Errorf("op[%d] delete rejected: %s", idx, s.denyReason(c, proj, br, configBitWrite))
			}
			repo, err := s.projects.Get(proj)
			if err != nil {
				return "", fmt.Errorf("op[%d] open project: %w", idx, err)
			}
			if err := repo.Delete(br, path); err != nil {
				return "", fmt.Errorf("op[%d] delete failed: %w", idx, err)
			}
			journal = append(journal, fmt.Sprintf("[%d] delete: %s@%s/%s", idx, proj, br, path))

		case "tag":
			project := op.Project
			name := op.Name
			ref := op.Ref
			if ref == "" {
				ref = defaultBranch
			}
			res, err := s.execTag(fmt.Sprintf("%s %s %s", project, name, ref), c, defaultBranch)
			if err != nil {
				return "", fmt.Errorf("op[%d] tag failed: %w", idx, err)
			}
			journal = append(journal, fmt.Sprintf("[%d] tag: %s", idx, res))

		case "commit":
			// 提交说明走 execCommit 自己的 " -m " 语法，
			// 不要给 BatchOp 留一个不生效的字段。
			target := op.Target
			if msg := strings.TrimSpace(op.Message); msg != "" {
				target = strings.TrimSpace(target + " -m " + msg)
			}
			res, err := s.execCommit(target, c, defaultBranch)
			if err != nil {
				return "", fmt.Errorf("op[%d] commit failed: %w", idx, err)
			}
			journal = append(journal, fmt.Sprintf("[%d] commit: %s", idx, res))

		default:
			return "", fmt.Errorf("op[%d] unknown action %q (allowed: put/cp/mv/delete/tag/commit)", idx, op.Action)
		}
	}

	_ = s.Reload()
	return fmt.Sprintf("executed operation list successfully (%d operations):\n  %s", len(journal), strings.Join(journal, "\n  ")), nil
}
