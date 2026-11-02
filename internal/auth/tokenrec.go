package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

// TokenRecord 是一张凭证的签发记录。
//
// 它不含密钥，也不参与验签——验签靠 JWT 自身的签名。
// 它存在的唯一理由是审计：半年后没人记得发过哪些凭证、发给了谁、
// 什么时候到期。删掉这个文件不影响该凭证能否使用，只影响你还能不能
// 找到它。
type TokenRecord struct {
	Name    string `json:"name"`
	JTI     string `json:"jti"`
	Sub     string `json:"sub"`
	Exp     int64  `json:"exp"`
	Created int64  `json:"created"`

	// Cap 与 Mount 记录签发时的能力上限与挂载表。
	//
	// 为什么要存下来：签发之后，这张凭证能干什么只存在于 JWT 载荷里，
	// 而 JWT 是不可读的（要验签才能解）。审计的人想知道「这张纸能写
	// 哪条分支」时，只剩两个选择——拿密钥解一遍，或者猜。
	//
	// 这两个字段**不参与验签**，改它不影响凭证能否使用。它们是记录，
	// 不是权威：权威永远是 JWT 载荷本身。
	//
	// 旧记录没有这两个字段，读出后是零值。零值在能力串里表示
	// 「不额外限制」，会让人误以为是一张全权凭证——所以用
	// CapRecorded/MountRecorded 区分「没记」和「记了空值」。
	Cap   string       `json:"cap,omitempty"`
	Mount []MountPoint `json:"mount,omitempty"`
	// CapRecorded 表示 Cap 字段可信；false 说明这是旧记录，能力未知。
	CapRecorded bool `json:"cap_recorded,omitempty"`
	// MountRecorded 表示 Mount 字段可信（含「空表 = 全命名空间」这一事实）。
	MountRecorded bool `json:"mount_recorded,omitempty"`

	// Gated 是签发时被按住的能力位（cap 的子集）。
	//
	// 这些位要生效必须有 WebUI 的临时授权。与 Claims.Gated 同源。
	Gated string `json:"gated,omitempty"`
}

// Expired 报告该凭证是否已过期。
func (r TokenRecord) Expired(now time.Time) bool {
	return r.Exp > 0 && now.Unix() > r.Exp
}

// TokenDir 返回某数据根下的签发记录目录。
func TokenDir(dataDir string) string { return filepath.Join(dataDir, "system", "tokens") }

// SaveTokenRecord 写入一条签发记录。同名记录会被覆盖。
func SaveTokenRecord(dataDir string, rec TokenRecord) error {
	dir := TokenDir(dataDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	if err := ValidateName(rec.Name); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, rec.Name+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit %s: %w", path, err)
	}
	return nil
}

// LoadTokenRecords 读出全部签发记录，按名字排序。
//
// 单条记录损坏只跳过它并收集错误，不让整个列表失败——
// 一条手改坏的 JSON 不应该让你看不见其余所有凭证。
func LoadTokenRecords(dataDir string) ([]TokenRecord, []error) {
	dir := TokenDir(dataDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{err}
	}
	var out []TokenRecord
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		var rec TokenRecord
		if err := json.Unmarshal(b, &rec); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// DeleteTokenRecord 删除一条签发记录。
func DeleteTokenRecord(dataDir, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(TokenDir(dataDir), name+".json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ValidateName 校验名字。它同时是文件名，所以不许路径分隔符。
//
// 具体规则只有一份实现（gitstore.ValidateName）——名字最终都会变成
// refs/ 或 repos/ 下的路径片段，判据必须同源。这里只做语义化包装，
// 让错误信息说清是「名字」而不是「ref」。
func ValidateName(name string) error { return gitstore.ValidateCredName(name) }

// ValidateRepoName 校验项目（裸仓库）名。与用户名不同，项目名允许斜杠
// ——"blackegg/me" 是合法的。
func ValidateRepoName(name string) error { return gitstore.ValidateRepoName(name) }

// ValidateBranchName 校验分支名。"*" 是挂载表里的通配声明，不是分支名，
// 调用方需要自己排除。
func ValidateBranchName(name string) error { return gitstore.ValidateBranch(name) }
