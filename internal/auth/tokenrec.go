package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
		return fmt.Errorf("建目录 %s: %w", dir, err)
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
		return fmt.Errorf("写 %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("提交 %s: %w", path, err)
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
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("名字不能为空")
	}
	if strings.ContainsAny(name, `/\:*?"<>|`) || strings.Contains(name, "..") {
		return fmt.Errorf("名字不能含路径分隔符或 : * ? \" < > |：%q", name)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("名字不能以 . 开头：%q", name)
	}
	return nil
}

// ValidateRepoName 校验项目（裸仓库）名。
//
// 与用户名不同，项目名允许斜杠——"blackegg/me" 是合法的。
// 但不允许空段、绝对路径、点段：那些会让它逃出 repos/ 目录。
func ValidateRepoName(name string) error {
	if name == "" {
		return fmt.Errorf("项目名不能为空")
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("项目名不能以 / 开头或结尾：%q", name)
	}
	if strings.ContainsAny(name, `\:*?"<>|`) {
		return fmt.Errorf("项目名不能含 \\ : * ? \" < > |：%q", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" {
			return fmt.Errorf("项目名不能有空的层级：%q", name)
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("项目名不能含 . 或 .. 段：%q", name)
		}
		if strings.HasPrefix(seg, ".") {
			return fmt.Errorf("项目名的每一层都不能以 . 开头：%q", name)
		}
	}
	return nil
}
