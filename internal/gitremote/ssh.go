package gitremote

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// SSHEndpoint 是解析后的 SSH 远端。
type SSHEndpoint struct {
	User string
	Host string
	Port string
	// Path 是仓库路径，已去掉开头的 /。
	Path string
}

// ParseSSHURL 解析两种 SSH 写法：
//
//	ssh://git@host:2222/org/repo.git
//	git@host:org/repo.git
//
// 不做任何猜测：解析不了就报错，而不是回落到「当作本地路径」。
func ParseSSHURL(raw string) (SSHEndpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return SSHEndpoint{}, errors.New("SSH address is empty")
	}
	if strings.HasPrefix(raw, "ssh://") {
		u, err := url.Parse(raw)
		if err != nil {
			return SSHEndpoint{}, fmt.Errorf("parse SSH address: %w", err)
		}
		ep := SSHEndpoint{User: u.User.Username(), Host: u.Hostname(), Port: u.Port(), Path: strings.TrimPrefix(u.Path, "/")}
		if ep.User == "" {
			ep.User = "git"
		}
		if ep.Port == "" {
			ep.Port = "22"
		}
		if ep.Host == "" || ep.Path == "" {
			return SSHEndpoint{}, fmt.Errorf("SSH address is missing host or path: %s", Redact(raw))
		}
		return ep, nil
	}
	at := strings.Index(raw, "@")
	if at <= 0 {
		return SSHEndpoint{}, fmt.Errorf("not an SSH address (missing user@): %s", Redact(raw))
	}
	user := raw[:at]
	rest := raw[at+1:]
	colon := strings.Index(rest, ":")
	if colon <= 0 {
		return SSHEndpoint{}, fmt.Errorf("not an SSH address (expected host:path): %s", Redact(raw))
	}
	host := rest[:colon]
	path := rest[colon+1:]
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.Trim(host, "[]")
	}
	ep := SSHEndpoint{User: user, Host: host, Port: "22", Path: path}
	if ep.Host == "" || ep.Path == "" {
		return SSHEndpoint{}, fmt.Errorf("SSH address is missing host or path: %s", Redact(raw))
	}
	return ep, nil
}

// Address 返回 host:port，供拨号用。
func (e SSHEndpoint) Address() string {
	return net.JoinHostPort(e.Host, e.Port)
}

// IsSSH 报告地址是否需要 SSH 传输。
func IsSSH(raw string) bool {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "ssh://") {
		return true
	}
	// scp 风格：user@host:path（scheme:// 形式已在上面排除）
	if strings.Contains(raw, "://") {
		return false
	}
	at := strings.Index(raw, "@")
	if at <= 0 {
		return false
	}
	return strings.Contains(raw[at:], ":")
}
