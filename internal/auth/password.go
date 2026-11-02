package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/argon2"
)

// PasswordStore 管的是 WebUI 管理口令。
//
// 为什么它必须在 <data>/system/ 里，而不是配置文件里：
//
//	配置文件对 MCP 侧是**可读**的——config show 原样回显当前生效配置，
//	而那条通道是 Agent 用的。口令写进配置就等于交给了 Agent。
//	签名密钥（jwt.key）早就按同一规矩办：独立文件、0600、永不回显。
//
// 存的是 argon2id 摘要，不是口令本身。摘要仍按 0600 处理：
// 摘要泄露只是把离线爆破的成本转嫁给攻击者，口令泄露则直接进系统。
type PasswordStore struct {
	systemDir string
}

// argon2id 参数。写死在代码里，同时自描述地存进摘要行：
// 换了参数旧摘要还能验。「换了参数所以没人登得上」是最难排查的一类故障。
const (
	pwTime    = 3
	pwMemory  = 64 * 1024 // KiB
	pwThreads = 2
	pwKeyLen  = 32
	pwSaltLen = 16
	pwAlgo    = "argon2id"
	// bootstrapBytes 是自动生成的初始口令的熵（base64 后约 24 字符）。
	bootstrapBytes = 18
)

// passwdFile 是摘要文件，pendingFile 表示「当前口令是引导口令，必须改」。
//
// 为什么用两个文件而不是在摘要里加标记：改密是一次原子替换，
// 而「必须改密」是另一个独立事实。两个文件各自原子，语义不会互相污染；
// 合并成一个就得在同一个文件里表达两种状态，改密时还要小心不要
// 顺手把标记一起清掉。
const (
	passwdFile  = "ui.passwd"
	pendingFile = "ui.passwd.pending"
)

func NewPasswordStore(systemDir string) *PasswordStore {
	return &PasswordStore{systemDir: systemDir}
}

func (p *PasswordStore) passwdPath() string  { return filepath.Join(p.systemDir, passwdFile) }
func (p *PasswordStore) pendingPath() string { return filepath.Join(p.systemDir, pendingFile) }

// Exists 报告是否已经设过口令。
func (p *PasswordStore) Exists() bool {
	_, err := os.Stat(p.passwdPath())
	return err == nil
}

// MustChange 报告当前口令是否还是引导口令。
func (p *PasswordStore) MustChange() bool {
	_, err := os.Stat(p.pendingPath())
	return err == nil
}

// HashPassword 生成自描述摘要行。
func HashPassword(pw string) (string, error) {
	if pw == "" {
		return "", fmt.Errorf("password must not be empty")
	}
	salt := make([]byte, pwSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, pwTime, pwMemory, pwThreads, pwKeyLen)
	return fmt.Sprintf("%s$v=1$t=%d$m=%d$p=%d$l=%d$%s$%s",
		pwAlgo, pwTime, pwMemory, pwThreads, pwKeyLen,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// CheckPassword 常数时间比较。摘要格式不认识时一律判假，不猜。
func CheckPassword(pw, sum string) bool {
	fields := strings.Split(strings.TrimSpace(sum), "$")
	if len(fields) != 8 || fields[0] != pwAlgo {
		return false
	}
	var t, m, p, l uint32
	if _, err := fmt.Sscanf(fields[2], "t=%d", &t); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(fields[3], "m=%d", &m); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(fields[4], "p=%d", &p); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(fields[5], "l=%d", &l); err != nil {
		return false
	}
	// 参数来自文件，必须设上限：一个被改成 m=4294967295 的摘要文件
	// 能让每次登录尝试吃掉几个 GB 内存，把服务自己打死。
	if t == 0 || t > 10 || m == 0 || m > 512*1024 || p == 0 || p > 16 || l == 0 || l > 128 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(fields[6])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(fields[7])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, uint8(p), l)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Verify 校验口令。
func (p *PasswordStore) Verify(pw string) bool {
	b, err := os.ReadFile(p.passwdPath())
	if err != nil {
		return false
	}
	return CheckPassword(pw, string(b))
}

// Set 写入新口令，并清掉「必须改密」标记。
//
// 先写临时文件再 rename：半截的摘要文件会让所有人都登不上，
// 而这个文件没有备份可退回。
func (p *PasswordStore) Set(pw string) error {
	sum, err := HashPassword(pw)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.systemDir, 0o750); err != nil {
		return fmt.Errorf("create directory %s: %w", p.systemDir, err)
	}
	path := p.passwdPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sum+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}

	// Preserve the old digest until the pending marker is removed. Otherwise
	// a failed marker removal leaves the new password installed but still
	// marked "must change"; the user can log in but every admin route keeps
	// denying them, while the old password has already been destroyed.
	old, readErr := os.ReadFile(path)
	hadOld := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		_ = os.Remove(tmp)
		return fmt.Errorf("read previous password digest: %w", readErr)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit %s: %w", path, err)
	}
	if err := os.Remove(p.pendingPath()); err != nil && !os.IsNotExist(err) {
		// Roll the digest back so callers can retry without being locked out.
		if hadOld {
			rollback := path + ".rollback"
			if werr := os.WriteFile(rollback, old, 0o600); werr == nil {
				_ = os.Rename(rollback, path)
			}
		} else {
			_ = os.Remove(path)
		}
		return fmt.Errorf("remove must-change marker: %w", err)
	}
	return nil
}

// Bootstrap 在首次启动时准备口令。
//
// explicit 非空时用它（调用方显式指定，不回显）；否则随机生成一个。
// 返回值 plain 只在**随机生成**时非空——调用方负责打印一次，
// 之后永不重印。已经设过口令或显式指定时返回空 plain。
//
// 为什么引导口令必须一次性：它会被打印到日志，而 docker logs 谁都能看。
// 它的价值不在保密，在于「第一次登录后就被换掉，日志里那个值当场作废」。
func (p *PasswordStore) Bootstrap(explicit string) (plain string, generated bool, err error) {
	if p.Exists() {
		return "", false, nil
	}
	pw := explicit
	if pw == "" {
		raw := make([]byte, bootstrapBytes)
		if _, err := rand.Read(raw); err != nil {
			return "", false, fmt.Errorf("generate bootstrap password: %w", err)
		}
		pw = base64.RawURLEncoding.EncodeToString(raw)
	}
	if err := p.Set(pw); err != nil {
		return "", false, err
	}
	// 标记「必须改密」。显式指定口令时同样要求改：
	// 一个写在 env / 文件里的口令迟早会留在部署配置里。
	if err := os.WriteFile(p.pendingPath(), []byte("1\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("write must-change marker: %w", err)
	}
	if explicit != "" {
		return "", true, nil
	}
	return pw, true, nil
}
