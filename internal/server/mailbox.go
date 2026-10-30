package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mail 是一条待办通知。
//
// 它不携带正文——正文在仓库里，这里只记「有事要做」。
// 只有外部事件（QQ 唤醒等）才带可选的 Body 摘要。
type Mail struct {
	Seq      int64     `json:"seq"`
	Addr     string    `json:"addr,omitempty"`
	Kind     string    `json:"kind,omitempty"`
	At       time.Time `json:"at"`
	Seen     bool      `json:"seen"`
	Endpoint string    `json:"endpoint"`
	To       string    `json:"to,omitempty"`
	From     string    `json:"from,omitempty"`
	Body     string    `json:"body,omitempty"`
}

// Mailbox 是实例级收件箱。
//
// 不分租户、不分身份：在新的单实例模型下，分身就是调用者自己，
// 收件箱就是一个普通的待办队列。抢到的人 ack，办完就消。
type Mailbox struct {
	mu    sync.Mutex
	seq   int64
	mails []Mail
}

func newMailbox() *Mailbox {
	return &Mailbox{}
}

// post 投一封信。
func (m *Mailbox) post(addr, kind, endpoint, body, to, from string) Mail {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	mail := Mail{
		Seq:      m.seq,
		Addr:     addr,
		Kind:     kind,
		At:       time.Now(),
		Endpoint: endpoint,
		Body:     body,
		To:       to,
		From:     from,
	}
	m.mails = append(m.mails, mail)
	if len(m.mails) > 200 {
		m.mails = m.mails[len(m.mails)-200:]
	}
	return mail
}

// pending 返回未确认的信。
func (m *Mailbox) pending() []Mail {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Mail
	for _, x := range m.mails {
		if !x.Seen {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// ack 确认一封信（seq=0 表示全清）。
func (m *Mailbox) ack(seq int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for i := range m.mails {
		if m.mails[i].Seen {
			continue
		}
		if seq == 0 || m.mails[i].Seq == seq {
			m.mails[i].Seen = true
			n++
		}
	}
	return n
}

// handleMailboxAPI 提供 HTTP GET/POST，给 pi 插件做热路径轮询与确认。
func (s *Server) handleMailboxAPI(w http.ResponseWriter, r *http.Request) {
	// 信箱必须鉴权：它直接暴露跨端点投递的内容，
	// 是唯一「不经过 MCP 也能读走消息」的入口。
	//
	// 早先这里没有任何校验，任何能访问端口的人都能读走全部信件——
	// 而 /mcp、/identity、/recall 都要求 Bearer，唯独信箱敞开，
	// 这种不一致比完全敞开更危险：它不会引起注意。
	caller, authErr := s.authenticate(r)
	if authErr != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="zero-memcp"`)
		http.Error(w, authErr.Error(), http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		pending := s.mailbox.pending()
		_ = json.NewEncoder(w).Encode(map[string]any{
			// mails 与 pending 同时给出：前者是历史字段名，插件在读；
			// 后者是 term 动词的措辞。两边都要，避免再出现
			// 「服务端发 mails、客户端读 pending」这种静默失配。
			"mails":   pending,
			"pending": pending,
			"count":   len(pending),
			"user":    caller.User,
		})

	case http.MethodPost:
		var req struct {
			Seq int64 `json:"seq"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&req)
		}
		n := s.mailbox.ack(req.Seq)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"acked":   n,
			"pending": len(s.mailbox.pending()),
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// execMailbox 处理 term 动词：mailbox [ack [N]]
func (s *Server) execMailbox(rest string) (TermResult, error) {
	parts := strings.Fields(rest)
	if len(parts) == 0 {
		pending := s.mailbox.pending()
		if len(pending) == 0 {
			return TermResult{
				Text:       "信箱为空",
				Structured: map[string]any{"count": 0, "mails": []any{}},
			}, nil
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("待办（%d 封）：\n", len(pending)))
		structMails := make([]any, 0, len(pending))
		for _, m := range pending {
			line := fmt.Sprintf("#%d [%s] %s (%s)",
				m.Seq, m.Kind, m.Addr, m.At.Format("15:04:05"))
			if m.Body != "" {
				line += " — " + m.Body
			}
			sb.WriteString(line + "\n")
			structMails = append(structMails, map[string]any{
				"seq":  m.Seq,
				"kind": m.Kind,
				"addr": m.Addr,
				"body": m.Body,
			})
		}
		return TermResult{
			Text: strings.TrimRight(sb.String(), "\n"),
			Structured: map[string]any{
				"count": len(pending),
				"mails": structMails,
			},
		}, nil
	}

	if parts[0] == "ack" {
		var seq int64
		if len(parts) > 1 {
			v, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return TermResult{}, fmt.Errorf("非法序号 %q", parts[1])
			}
			seq = v
		}
		n := s.mailbox.ack(seq)
		return TermResult{
			Text: fmt.Sprintf("已确认 %d 封", n),
			Structured: map[string]any{
				"acked":   n,
				"pending": len(s.mailbox.pending()),
			},
		}, nil
	}

	return TermResult{}, fmt.Errorf("未知 mailbox 子命令 %q（只接受 ack 或空）", parts[0])
}
