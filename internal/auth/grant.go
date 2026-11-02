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

// Grant 是一条临时写授权。
//
// 存在理由是「临时放 agent 改记忆，改完收回」：
// 凭证的 cap/mount 在签发时定死，改配置要重启且影响所有用同一用户的凭证。
// 而放权这件事应该——立即生效、只针对一张凭证、只针对某条分支、
// 到期自动失效、随时能撤销。
//
// 它只能**加**，不能减：要收回已经给的写权限用分支锁（lock），
// 那是另一套语义（对整个分支生效，不针对某张凭证）。
// 两套机制各管一件事，不要试图用一个表达另一个。
type Grant struct {
	// JTI 是获得授权的凭证编号。授权绑定凭证而不是用户：
	// 同一个用户可能有多张凭证，给 laptop 放权不该等于给 pi-box 放权。
	JTI string `json:"jti"`
	// Project 是项目名，Branch 是分支名。"*" 表示该项目所有分支。
	Project string `json:"project"`
	Branch  string `json:"branch"`
	// Cap 是这次临时放行的能力位，必须是凭证 Gated 与 cap 的子集。
	Cap string `json:"cap"`
	// Expires 是到期时间戳，0 表示不过期（但仍可显式撤销）。
	Expires int64 `json:"expires,omitempty"`
	// GrantedBy 是授权人（WebUI 口令持有者的标记），审计用。
	GrantedBy string `json:"granted_by,omitempty"`
	// GrantedAt 是授权时间戳。
	GrantedAt int64 `json:"granted_at,omitempty"`
}

// Expired 报告该授权是否已过期。
func (g Grant) Expired(now time.Time) bool {
	return g.Expires > 0 && now.Unix() > g.Expires
}

// Covers 报告该授权是否覆盖 (project, branch) 上的某位能力。
func (g Grant) Covers(project, branch string, bit rune) bool {
	if g.Expired(time.Now()) {
		return false
	}
	if !strings.ContainsRune(g.Cap, bit) {
		return false
	}
	if g.Project != project {
		return false
	}
	return g.Branch == branch || g.Branch == "*"
}

// GrantStore 是授权记录的存放处。
//
// 为什么是一张凭证一个文件：撤销单张凭证的授权是一次 unlink，
// 不需要读-改-写整个集合。而「读-改-写」在并发下会丢更新，
// 且失败时可能写坏所有人的授权。
type GrantStore struct {
	dir string
}

// NormalizeCapabilitySet validates a capability set and returns it in rwx order.
// Duplicate bits are collapsed. Empty input remains empty for callers that need
// to distinguish an omitted optional cap from an invalid set.
func NormalizeCapabilitySet(cap string) (string, error) {
	var out strings.Builder
	for _, bit := range "rwx" {
		if strings.ContainsRune(cap, bit) {
			out.WriteRune(bit)
		}
	}
	for _, bit := range cap {
		if !strings.ContainsRune("rwx", bit) {
			return "", fmt.Errorf("capability %q may contain only rwx", cap)
		}
	}
	return out.String(), nil
}

func NewGrantStore(dataDir string) *GrantStore {
	return &GrantStore{dir: filepath.Join(dataDir, "system", "grants")}
}

func (g *GrantStore) pathFor(jti string) string {
	return filepath.Join(g.dir, jti+".json")
}

// List 读出某张凭证的全部授权。文件不存在返回空。
func (g *GrantStore) List(jti string) ([]Grant, error) {
	if err := ValidateJTI(jti); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(g.pathFor(jti))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Grant
	if err := json.Unmarshal(b, &out); err != nil {
		// 坏文件不该让整张凭证变成「有全部权限」或「完全没权限」。
		// 返回错误，调用方按「没有授权」处理——失败方向必须是拒绝。
		return nil, fmt.Errorf("parse grant records for %s: %w", jti, err)
	}
	return out, nil
}

// Allowed 报告某张凭证在 (project, branch) 上是否被临时授予了 bit。
//
// 每次判定都读盘，不做缓存：与吊销名单（Revocations.Revoked）同一套
// 做法。缓存会让「刚收回的权限还能用一会儿」，而权限回收必须是即时的。
func (g *GrantStore) Allowed(jti, project, branch string, bit rune) bool {
	grants, err := g.List(jti)
	if err != nil {
		return false
	}
	for _, gr := range grants {
		if gr.Covers(project, branch, bit) {
			return true
		}
	}
	return false
}

// Grant 追加或替换某张凭证在某项目/分支上的授权。
// 同一 JTI + project + branch 只保留一条记录；再次授权会替换能力集合与期限。
func (g *GrantStore) Grant(in Grant) error {
	if err := ValidateJTI(in.JTI); err != nil {
		return err
	}
	cap, err := NormalizeCapabilitySet(in.Cap)
	if err != nil {
		return fmt.Errorf("invalid grant capability: %w", err)
	}
	if cap == "" {
		return fmt.Errorf("grant capability is required")
	}
	in.Cap = cap
	if in.Project == "" {
		return fmt.Errorf("grant project is required")
	}
	if in.GrantedAt == 0 {
		in.GrantedAt = time.Now().Unix()
	}

	existing, err := g.List(in.JTI)
	if err != nil {
		return err
	}
	out := make([]Grant, 0, len(existing)+1)
	for _, e := range existing {
		if e.Project == in.Project && e.Branch == in.Branch {
			continue
		}
		out = append(out, e)
	}
	out = append(out, in)
	return g.write(in.JTI, out)
}

// Revoke 撤销某张凭证在 (project, branch) 上的授权。
// project 为空表示撤销该凭证的全部授权。
func (g *GrantStore) Revoke(jti, project, branch string) (int, error) {
	existing, err := g.List(jti)
	if err != nil {
		return 0, err
	}
	if len(existing) == 0 {
		return 0, nil
	}
	if project == "" {
		if err := os.Remove(g.pathFor(jti)); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		return len(existing), nil
	}
	out := make([]Grant, 0, len(existing))
	removed := 0
	for _, e := range existing {
		if e.Project == project && (branch == "" || e.Branch == branch) {
			removed++
			continue
		}
		out = append(out, e)
	}
	if removed == 0 {
		return 0, nil
	}
	if len(out) == 0 {
		if err := os.Remove(g.pathFor(jti)); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		return removed, nil
	}
	return removed, g.write(jti, out)
}

// ListAll 读出全部凭证的授权，按 JTI 排序。
func (g *GrantStore) ListAll() (map[string][]Grant, error) {
	entries, err := os.ReadDir(g.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string][]Grant{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		jti := strings.TrimSuffix(e.Name(), ".json")
		grants, gerr := g.List(jti)
		if gerr != nil {
			continue
		}
		if len(grants) > 0 {
			out[jti] = grants
		}
	}
	return out, nil
}

// write 原子替换某张凭证的授权文件。
func (g *GrantStore) write(jti string, grants []Grant) error {
	if err := os.MkdirAll(g.dir, 0o750); err != nil {
		return fmt.Errorf("create directory %s: %w", g.dir, err)
	}
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].Project != grants[j].Project {
			return grants[i].Project < grants[j].Project
		}
		return grants[i].Branch < grants[j].Branch
	})
	b, err := json.MarshalIndent(grants, "", "  ")
	if err != nil {
		return err
	}
	path := g.pathFor(jti)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}

// ValidateMountCap 校验一个挂载点的能力是否在凭证能力范围内。
//
// 空串在能力串里的含义是「不额外限制」，所以：
//   - 挂载点不写 cap：继承凭证级 cap，永远合法
//   - 凭证级不写 cap（= rwx）：任何挂载点 cap 都是它的子集
//   - 两边都写了：挂载点必须是子集
//
// 剩下的组合（两边都写了但不相交）在运行期会被算成空集，
// 而空集在该体系里另有含义（不额外限制）——签出这种凭证
// 等于把一个矛盾藏到权限判定里去。宁可现在拒绝。
func ValidateMountCap(credCap string, pt MountPoint) error {
	if pt.Cap == "" {
		return nil
	}
	for _, ch := range pt.Cap {
		if !strings.ContainsRune("rwx", ch) {
			return fmt.Errorf("mount point %s cap %q may only contain rwx", pt.Project, pt.Cap)
		}
	}
	if credCap == "" {
		return nil
	}
	for _, ch := range pt.Cap {
		if !strings.ContainsRune(credCap, ch) {
			return fmt.Errorf("mount point %s cap %q exceeds credential capability %q (a mount may narrow, never widen)",
				pt.Project, pt.Cap, credCap)
		}
	}
	return nil
}
