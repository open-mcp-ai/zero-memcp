package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"golang.org/x/crypto/ssh"
)

func (s *Server) sshKeyDir() string { return filepath.Join(s.dataDir, "system", "sshkeys") }

// putSSHKey stores a private key without ever echoing it into tool output.
func (s *Server) putSSHKey(name, text string) error {
	if err := config.ValidateSSHKeyName(name); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if _, err := ssh.ParsePrivateKey([]byte(text)); err != nil {
		return fmt.Errorf("SSH private key must be an unencrypted OpenSSH key: %w", err)
	}
	if err := os.MkdirAll(s.sshKeyDir(), 0o700); err != nil {
		return err
	}
	dirInfo, err := os.Lstat(s.sshKeyDir())
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("SSH key directory must be a non-symlink directory")
	}
	if err := os.Chmod(s.sshKeyDir(), 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.sshKeyDir(), name+".pem")
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("SSH private key %q already exists; run remote-key delete before replacing it with put", name)
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(s.sshKeyDir(), ".remote-key-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(text + "\n"); err != nil {
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
	return syncDirectory(s.sshKeyDir())
}

func (s *Server) listSSHKeys() ([]string, error) {
	entries, err := os.ReadDir(s.sshKeyDir())
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pem") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".pem"))
	}
	return names, nil
}

func (s *Server) deleteSSHKey(name string) error {
	if err := config.ValidateSSHKeyName(name); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.sshKeyDir(), name+".pem")); err != nil {
		return err
	}
	return nil
}

func (s *Server) readSSHKey(name string) ([]byte, error) {
	if err := config.ValidateSSHKeyName(name); err != nil {
		return nil, err
	}
	dirInfo, err := os.Lstat(s.sshKeyDir())
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("invalid SSH key directory")
	}
	path := filepath.Join(s.sshKeyDir(), name+".pem")
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("SSH private key must be a regular file")
	}
	if st.Size() > 64<<10 {
		return nil, errors.New("SSH private key file exceeds 64 KiB")
	}
	if st.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("SSH private key permissions are not 0600: %04o", st.Mode().Perm())
	}
	return os.ReadFile(path)
}

func (s *Server) execRemoteKey(rest string, c Caller) (TermResult, error) {
	if err := s.requireRootGlobal(c, 'w'); err != nil {
		return TermResult{}, err
	}
	rest = strings.TrimSpace(rest)
	if rest == "" || rest == "list" {
		names, err := s.listSSHKeys()
		if err != nil {
			return TermResult{}, err
		}
		return TermResult{Text: strings.Join(names, "\n"), Structured: map[string]any{"keys": names}}, nil
	}
	if strings.HasPrefix(rest, "put ") {
		arg := strings.TrimSpace(strings.TrimPrefix(rest, "put"))
		id, payload, ok := strings.Cut(arg, "--")
		if !ok {
			return TermResult{}, errors.New("usage: remote-key put <name> -- $PRIVATE_KEY (accepts unencrypted OpenSSH keys only)")
		}
		id = strings.TrimSpace(id)
		payload = strings.TrimSpace(restorePayload(strings.TrimPrefix(payload, " ")))
		if payload == "" {
			return TermResult{}, errors.New("private key is empty")
		}
		if err := s.putSSHKey(id, payload); err != nil {
			return TermResult{}, err
		}
		return TermResult{Text: fmt.Sprintf("SSH private key %q saved securely; contents are not echoed", id), Structured: map[string]any{"ok": true, "name": id}}, nil
	}
	fields := strings.Fields(rest)
	if len(fields) == 2 && (fields[0] == "delete" || fields[0] == "rm") {
		if err := s.deleteSSHKey(fields[1]); err != nil {
			return TermResult{}, err
		}
		return TermResult{Text: fmt.Sprintf("SSH private key %q deleted", fields[1]), Structured: map[string]any{"ok": true, "deleted": fields[1]}}, nil
	}
	return TermResult{}, errors.New("usage: remote-key list | put <name> -- $PRIVATE_KEY | delete <name>")
}
