package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/open-mcp-ai/zero-memcp/internal/config"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 主人密钥体系：多公钥 + 权限位 + 根钥派生。
//
// # 为什么公钥只能从配置文件和 WebUI 进来，绝不能走 MCP
//
// 如果 agent 能通过 MCP 添加公钥，它就能自己生成一对密钥、把自己那把
// 加进去，然后给自己签任何授权——两道门立刻塌成一道。
// 所以写入入口只有两个，都要求「人的身份」：
//
//	① 配置文件 [[auth.key]]：要能改文件，等于已经能控制这台机器；
//	② WebUI：要人工登录（会话密码），不是 MCP 凭证。
//
// # 两类钥
//
//	根钥（root = true）  能批准所有操作，**并且能签发/吊销派生钥**。
//	派生钥（caps = [...]） 只能批准 caps 里列出的操作类别。
//
// 派生钥的记录存在 data/system/masterkeys.json，每条都带**根钥签名**。
// 服务端只信「能验通根钥签名」的记录——于是「根钥添加其他公钥」这件事
// 不需要任何额外的信任假设：没有根私钥就写不进去。

// 操作类别（权限位）。刻意用语义名而不是 r/w/x：
// 销毁数据、改注入提示的规则、改权限表，这三件事的风险完全不同，
// 用一个 x 位把它们打包在一起，等于逼人要么全给、要么全不给。
const (
	capErase   = "erase"
	capReplace = "replace"
	capRule    = "rule" // 改 SKILL.md 这类注入系统提示的文件
	capACL     = "acl"  // 改用户/权限
	capKeys    = "keys" // 管理主人密钥（仅根钥隐含）
)

var validCaps = map[string]bool{
	capErase: true, capReplace: true, capRule: true, capACL: true, capKeys: true,
}

// masterKey 是一把已生效的主人公钥。
type masterKey struct {
	Name string
	Pub  ed25519.PublicKey
	Caps map[string]bool
	Root bool
	// Source 标明它从哪来（config / delegated），出现在审计与 status 里。
	Source string
}

func (k masterKey) allows(action string) bool {
	if k.Root {
		return true
	}
	return k.Caps[action]
}

// keyRecord 是派生钥的存储形态，自带签发者签名。
type keyRecord struct {
	Name      string   `json:"name"`
	PublicKey string   `json:"public_key"`
	Caps      []string `json:"caps,omitempty"`
	Root      bool     `json:"root,omitempty"`
	IssuedBy  string   `json:"issued_by"`
	IssuedAt  int64    `json:"issued_at"`
	Revoked   bool     `json:"revoked,omitempty"`
	Signature string   `json:"signature"`
}

// keyRecordPayload 是参与签名的规范化串（版本前缀写死）。
func keyRecordPayload(r keyRecord) string {
	caps := append([]string(nil), r.Caps...)
	sort.Strings(caps)
	return fmt.Sprintf("zero-memcp-key-v1\n%s\n%s\n%s\n%t\n%s\n%d\n%t",
		r.Name, r.PublicKey, strings.Join(caps, ","), r.Root, r.IssuedBy, r.IssuedAt, r.Revoked)
}

// masterKeyring 是当前生效的密钥集合。
//
// 读多写少（每次危险操作读一次，改钥时整体替换），所以用
// atomic.Pointer 发布不可变快照，不加锁。
type masterKeyring struct {
	keys     atomic.Pointer[[]masterKey]
	filePath string
	// issued / used 是一张票据只能使用一次的记录。
	issued sync.Map // nonce -> expiresUnix
	used   sync.Map // nonce -> usedUnix
}

// keys 返回当前生效的密钥快照。
func (k *masterKeyring) snapshot() []masterKey {
	if p := k.keys.Load(); p != nil {
		return *p
	}
	return nil
}

func (k *masterKeyring) count() int { return len(k.snapshot()) }

// describeLines 给 status 列出钥与权限位（不泄露私钥——本来也没有）。
func (k *masterKeyring) describeLines() []string {
	ks := k.snapshot()
	if len(ks) == 0 {
		return nil
	}
	out := make([]string, 0, len(ks))
	for _, mk := range ks {
		caps := "all (root)"
		if !mk.Root {
			names := make([]string, 0, len(mk.Caps))
			for c := range mk.Caps {
				names = append(names, c)
			}
			sort.Strings(names)
			caps = strings.Join(names, ",")
			if caps == "" {
				caps = "none"
			}
		}
		out = append(out, fmt.Sprintf("%s [%s] caps=%s", mk.Name, mk.Source, caps))
	}
	return out
}

// authorize 找出能验通本次签名、且允许该操作的那把钥。
//
// 关键点：**先看权限位再验签没有意义**（谁都能声称自己是某把钥），
// 反过来「验通了但没权限」才是正确答案。所以实现是：
// 逐把钥用公钥验签，验通的唯一那把（签名只能对应一把钥）再看权限位。
func (k *masterKeyring) authorize(action string, payload string, sigB64 string) (string, error) {
	ks := k.snapshot()
	if len(ks) == 0 {
		return "", fmt.Errorf("no master key configured; refusing %q. Add one in the config file or via the WebUI", action)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil {
		return "", fmt.Errorf("signature is not valid base64: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return "", fmt.Errorf("signature has %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	var matched *masterKey
	for i := range ks {
		if ed25519.Verify(ks[i].Pub, []byte(payload), sig) {
			matched = &ks[i]
			break
		}
	}
	if matched == nil {
		return "", fmt.Errorf("signature does not verify against any configured master key")
	}
	if !matched.allows(action) {
		return "", fmt.Errorf("key %q is not allowed to authorize %q (its caps do not include it)", matched.Name, action)
	}
	return matched.Name, nil
}

// masterKeysPath 是派生钥的落盘位置。
func (s *Server) masterKeysPath() string {
	return filepath.Join(s.dataDir, "system", "masterkeys.json")
}

// masterKeysDir 是按文件放主人公钥的目录：<data>/keys/。
//
// 为什么从单一大配置拆成「一钥一文件」：
// 加/换/撤一把设备公钥只需要增删一个文件，不用碰 zero-memcp.toml；
// 与 system/sshkeys 同理，目录与文件权限必须收紧到属主可写，
// 若被同机别的用户或 group/world 可写，直接拒载并报警——
// 否则一个失陷的非特权进程只要丢个文件进来就成了「主人」。
func (s *Server) masterKeysDir() string {
	return filepath.Join(s.dataDir, "keys")
}

// loadKeysDir 从 <data>/keys/*.toml 读出配置期的对等钥与派生钥。
//
// 为什么用 TOML 而不是 JSON：配置文件、规则注释、人眼排查都与
// zero-memcp.toml 同一种语法，AI 读起来也零噪音、允许写注释说明
// 「这把钥是谁、发给哪台设备、为什么只给这几个权限」。
func (s *Server) loadKeysDir(baseKeys *[]masterKey, seen map[string]bool) []keyRecord {
	dir := s.masterKeysDir()
	info, err := os.Lstat(dir)
	if err != nil {
		return nil // 目录不存在不是错，大多数部署根本不用这个功能
	}
	if !info.IsDir() {
		s.logf("master-keys: %s is not a directory — ignored", dir)
		return nil
	}
	// 权限守门：只在能用 Unix 权限位的平台上做。
	// Windows 的 Mode().Perm() 恒为 0666/0777，照搬判断等于拒绝一切。
	if permsVerifiable() && permTooOpen(info.Mode()) {
		s.logf("master-keys: REFUSING %s (mode %04o is group/world-writable; %s)",
			dir, info.Mode().Perm(), permHint())
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.logf("master-keys: read %s: %v", dir, err)
		return nil
	}
	var delegated []keyRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		fpath := filepath.Join(dir, e.Name())
		fi, serr := os.Lstat(fpath)
		if serr != nil || (permsVerifiable() && permTooOpen(fi.Mode())) {
			s.logf("master-keys: REFUSING %s (writable by group/others; %s)", fpath, permHint())
			continue
		}
		raw, rerr := os.ReadFile(fpath)
		if rerr != nil {
			s.logf("master-keys: read %s: %v", fpath, rerr)
			continue
		}
		rec, perr := parseKeyTOML(raw, strings.TrimSuffix(e.Name(), ".toml"))
		if perr != nil {
			s.logf("master-keys: %s: %v", fpath, perr)
			continue
		}
		// 带签发者的属于「派生钥」，走根钥验签链；
		// 没写签发者的直接作为目录信任锚（对等钥 / 受限权限钥），由文件系统权限背书。
		if rec.IssuedBy != "" {
			delegated = append(delegated, rec)
			continue
		}
		pub, derr := decodeEd25519(rec.PublicKey)
		if derr != nil {
			s.logf("master-keys: %s: %v", fpath, derr)
			continue
		}
		caps := map[string]bool{}
		for _, c := range rec.Caps {
			c = strings.ToLower(strings.TrimSpace(c))
			if !validCaps[c] {
				s.logf("master-keys: %s unknown cap %q (ignored)", fpath, c)
				continue
			}
			caps[c] = true
		}
		if !seen[rec.Name] {
			*baseKeys = append(*baseKeys, masterKey{
				Name: rec.Name, Pub: pub, Caps: caps, Root: rec.Root, Source: "keys/" + e.Name(),
			})
			seen[rec.Name] = true
		}
	}
	return delegated
}

// parseKeyTOML 解析单把钥的 TOML 文件（允许顶层裸键值对，也允许放在 [key] 段下）。
func parseKeyTOML(raw []byte, defaultName string) (keyRecord, error) {
	doc, err := config.Parse(raw)
	if err != nil {
		return keyRecord{}, err
	}
	// 密钥文件统一用 [key] 段：一个文件一把钥，段名就是「这是一把钥」。
	// 我们的 TOML 解析器要求先声明分节（服务端配置一直是这个形状），
	// 这里顺着它，而不是为一个新文件类型引入第二套解析规则。
	sec, ok := doc.Sections["key"]
	if !ok || len(sec) == 0 {
		return keyRecord{}, fmt.Errorf("missing [key] section (each file declares exactly one key)")
	}
	var rec keyRecord
	if v, ok := sec["name"]; ok {
		rec.Name = v.Str
	}
	if rec.Name == "" {
		rec.Name = defaultName
	}
	if v, ok := sec["public_key"]; ok {
		rec.PublicKey = v.Str
	}
	if v, ok := sec["caps"]; ok {
		rec.Caps = v.List
	}
	if v, ok := sec["root"]; ok {
		rec.Root = v.Bool
	}
	if v, ok := sec["issued_by"]; ok {
		rec.IssuedBy = v.Str
	}
	if v, ok := sec["issued_at"]; ok {
		rec.IssuedAt = v.Int
	}
	if v, ok := sec["revoked"]; ok {
		rec.Revoked = v.Bool
	}
	if v, ok := sec["signature"]; ok {
		rec.Signature = v.Str
	}
	return rec, nil
}

// loadMasterKeys 载入配置里的钥 + keys/ 目录 + 磁盘上的派生钥。
//
// 派生钥必须以「能被某个已生效根钥验通」为前提，否则一律丢弃并出声——
// 静默忽略一条伪造记录，等于假装这台机器没被写过东西。
func (s *Server) loadMasterKeys() {
	var keys []masterKey
	seen := map[string]bool{}

	// ① 配置里的钥（信任锚）
	cf := s.cfgFilePtr.Load()
	if cf != nil {
		for _, k := range cf.AuthKeys {
			pub, err := decodeEd25519(k.PublicKey)
			if err != nil {
				s.logf("master-keys: config key %q ignored: %v", k.Name, err)
				continue
			}
			caps := map[string]bool{}
			for _, c := range k.Caps {
				c = strings.ToLower(strings.TrimSpace(c))
				if !validCaps[c] {
					s.logf("master-keys: config key %q has unknown capability %q (ignored)", k.Name, c)
					continue
				}
				caps[c] = true
			}
			keys = append(keys, masterKey{
				Name: k.Name, Pub: pub, Caps: caps, Root: k.Root, Source: "config",
			})
			seen[k.Name] = true
		}
	}

	// ② <data>/keys/*.json：一钥一文件（不用改主配置文件）
	dirDelegated := s.loadKeysDir(&keys, seen)

	// ③ 磁盘上的派生钥 + 目录里带签发者的派生钥：统一验证根钥签名
	var recs []keyRecord
	recs = append(recs, dirDelegated...)
	raw, err := os.ReadFile(s.masterKeysPath())
	if err == nil {
		var fileRecs []keyRecord
		if jerr := json.Unmarshal(raw, &fileRecs); jerr != nil {
			s.logf("master-keys: %s is not valid JSON, ignoring (%v)", s.masterKeysPath(), jerr)
		} else {
			recs = append(recs, fileRecs...)
		}
	}
	if len(recs) > 0 {
		{
			sort.Slice(recs, func(i, j int) bool { return recs[i].IssuedAt < recs[j].IssuedAt })
			effective := map[string]masterKey{}
			for _, r := range recs {
				issuer, ok := findRoot(keys, effective, r.IssuedBy)
				if !ok {
					s.logf("master-keys: record %q names unknown issuer %q — ignored", r.Name, r.IssuedBy)
					continue
				}
				sig, derr := base64.StdEncoding.DecodeString(r.Signature)
				if derr != nil || !ed25519.Verify(issuer.Pub, []byte(keyRecordPayload(r)), sig) {
					s.logf("master-keys: record %q has an invalid issuer signature — ignored", r.Name)
					continue
				}
				if r.Revoked {
					delete(effective, r.Name)
					continue
				}
				pub, perr := decodeEd25519(r.PublicKey)
				if perr != nil {
					s.logf("master-keys: record %q has an invalid public key — ignored", r.Name)
					continue
				}
				caps := map[string]bool{}
				for _, c := range r.Caps {
					if validCaps[strings.ToLower(c)] {
						caps[strings.ToLower(c)] = true
					}
				}
				effective[r.Name] = masterKey{
					Name: r.Name, Pub: pub, Caps: caps, Root: r.Root, Source: "delegated:" + r.IssuedBy,
				}
			}
			for name, mk := range effective {
				if !seen[name] {
					keys = append(keys, mk)
					seen[name] = true
				}
			}
		}
	}

	if s.masterKeys == nil {
		s.masterKeys = &masterKeyring{}
	}
	s.masterKeys.keys.Store(&keys)
	if len(keys) > 0 {
		s.logf("master-keys: %d key(s) loaded: %s", len(keys), strings.Join(s.masterKeys.describeLines(), "; "))
	}
}

func findRoot(configured []masterKey, effective map[string]masterKey, name string) (masterKey, bool) {
	for _, k := range configured {
		if k.Name == name && k.Root {
			return k, true
		}
	}
	if k, ok := effective[name]; ok && k.Root {
		return k, true
	}
	return masterKey{}, false
}

func decodeEd25519(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("not base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("has %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// addDelegatedKey 记录一条派生钥（必须带根钥签名）。
//
// 唯一入口是 WebUI 的已登录会话；不进 MCP 动词是刻意的。
func (s *Server) addDelegatedKey(rec keyRecord) error {
	if rec.Name == "" || rec.PublicKey == "" || rec.IssuedBy == "" || rec.Signature == "" {
		return fmt.Errorf("record needs name, public_key, issued_by and signature")
	}
	if _, err := decodeEd25519(rec.PublicKey); err != nil {
		return fmt.Errorf("public_key: %w", err)
	}
	for _, c := range rec.Caps {
		if !validCaps[strings.ToLower(strings.TrimSpace(c))] {
			return fmt.Errorf("unknown capability %q (valid: erase, replace, rule, acl, keys)", c)
		}
	}
	if rec.IssuedAt == 0 {
		rec.IssuedAt = time.Now().Unix()
	}
	// 先验证签发者确实是根钥
	issuer, ok := findRoot(s.masterKeys.snapshot(), nil, rec.IssuedBy)
	if !ok {
		return fmt.Errorf("issued_by %q is not a configured root key", rec.IssuedBy)
	}
	sig, err := base64.StdEncoding.DecodeString(rec.Signature)
	if err != nil || !ed25519.Verify(issuer.Pub, []byte(keyRecordPayload(rec)), sig) {
		return fmt.Errorf("signature does not verify against root key %q", rec.IssuedBy)
	}

	existing, err := s.readKeyRecords()
	if err != nil {
		return err
	}
	existing = append(existing, rec)
	if err := s.writeKeyRecords(existing); err != nil {
		return err
	}
	s.loadMasterKeys()
	return nil
}

// revokeDelegatedKey 追加一条吊销记录（同样是根钥签名）。
func (s *Server) revokeDelegatedKey(rec keyRecord) error {
	rec.Revoked = true
	rec.Caps = nil
	return s.addDelegatedKey(rec)
}

func (s *Server) readKeyRecords() ([]keyRecord, error) {
	raw, err := os.ReadFile(s.masterKeysPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var recs []keyRecord
	if err := json.Unmarshal(raw, &recs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.masterKeysPath(), err)
	}
	return recs, nil
}

func (s *Server) writeKeyRecords(recs []keyRecord) error {
	dir := filepath.Dir(s.masterKeysPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.masterKeysPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.masterKeysPath())
}

// ticketExpiry 取回某 nonce 已登记的过期时间（票据由本服务签发）。
func (s *Server) ticketExpiry(nonce string) int64 {
	if s.masterKeys == nil {
		return 0
	}
	if v, ok := s.masterKeys.issued.Load(nonce); ok {
		return v.(int64)
	}
	return 0
}
