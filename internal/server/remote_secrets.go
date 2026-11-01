package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
)

// HTTP token 的存放与 SSH 私钥分开：
//
// 私钥是 key（有格式、有用途），token 是任意字符串（GitHub 的 PAT 可能很长，
// 也可能只是个随机串）。混在一个目录里会让「这是私钥还是口令」
// 只能靠文件名猜，而猜错的后果是拿一段 token 去当私钥解析。

func (s *Server) secretDir() string { return filepath.Join(s.dataDir, "system", "secrets") }

func (s *Server) putSecret(name, value string) error {
	if err := config.ValidateSSHKeyName(name); err != nil {
		return err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("token 为空")
	}
	if strings.ContainsAny(value, "\r\n") {
		return errors.New("token 不能包含换行")
	}
	if len(value) > 8<<10 {
		return errors.New("token 太长（上限 8 KiB）")
	}
	if err := os.MkdirAll(s.secretDir(), 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(s.secretDir())
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("secret 目录必须是非符号链接目录")
	}
	if err := os.Chmod(s.secretDir(), 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.secretDir(), name)
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("token %q 已存在；要轮换请先 remote-secret delete", name)
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(s.secretDir(), ".secret-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(value + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDirectory(s.secretDir())
}

func (s *Server) listSecrets() ([]string, error) {
	entries, err := os.ReadDir(s.secretDir())
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	return names, nil
}

func (s *Server) deleteSecret(name string) error {
	if err := config.ValidateSSHKeyName(name); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.secretDir(), name)); err != nil {
		return err
	}
	return nil
}

func (s *Server) readSecret(name string) (string, error) {
	if err := config.ValidateSSHKeyName(name); err != nil {
		return "", err
	}
	path := filepath.Join(s.secretDir(), name)
	st, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("token 必须是普通文件")
	}
	if st.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("token 权限不是 0600：%04o", st.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// execRemoteSecret 管理 HTTP 推送用的 token。token 永不回显。
func (s *Server) execRemoteSecret(rest string, c Caller) (TermResult, error) {
	if err := s.requireRootGlobal(c, 'w'); err != nil {
		return TermResult{}, err
	}
	rest = strings.TrimSpace(rest)
	if rest == "" || rest == "list" {
		names, err := s.listSecrets()
		if err != nil {
			return TermResult{}, err
		}
		return TermResult{Text: strings.Join(names, "\n"), Structured: map[string]any{"secrets": names}}, nil
	}
	if strings.HasPrefix(rest, "put ") {
		arg := strings.TrimSpace(strings.TrimPrefix(rest, "put"))
		id, payload, ok := strings.Cut(arg, "--")
		if !ok {
			return TermResult{}, errors.New("用法：remote-secret put <name> -- $TOKEN")
		}
		id = strings.TrimSpace(id)
		payload = strings.TrimSpace(restorePayload(strings.TrimPrefix(payload, " ")))
		if err := s.putSecret(id, payload); err != nil {
			return TermResult{}, err
		}
		return TermResult{Text: fmt.Sprintf("token %q 已安全保存；内容不会回显", id), Structured: map[string]any{"ok": true, "name": id}}, nil
	}
	fields := strings.Fields(rest)
	if len(fields) == 2 && (fields[0] == "delete" || fields[0] == "rm") {
		if err := s.deleteSecret(fields[1]); err != nil {
			return TermResult{}, err
		}
		return TermResult{Text: fmt.Sprintf("token %q 已删除", fields[1]), Structured: map[string]any{"ok": true, "deleted": fields[1]}}, nil
	}
	return TermResult{}, errors.New("用法：remote-secret list | put <name> -- $TOKEN | delete <name>")
}

// optionalSecret 读出可选的凭证：没声明就匿名，声明了但读不出来就报错。
//
// 这个区分很重要：匿名拉公开仓库是正常用法，而「声明了 token 却读不到」
// 是配置错误——两者都退回匿名的话，一个拼错的 secret 名会变成
// 「远端说 404」，让人查半天网络。
func (s *Server) optionalSecret(name, field string) (string, error) {
	if name == "" {
		return "", nil
	}
	token, err := s.readSecret(name)
	if err != nil {
		return "", fmt.Errorf("读取 %s %q 失败: %w", field, name, err)
	}
	return token, nil
}
