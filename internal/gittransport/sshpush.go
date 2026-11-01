// Package gittransport implements the small part of Git's SSH transport that
// zero-memcp needs for an explicit, single-branch push.
package gittransport

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-mcp-ai/zero-memcp/internal/gitstore"
)

const zeroSHA = "0000000000000000000000000000000000000000"

// PushResult identifies the ref update acknowledged by receive-pack.
type PushResult struct {
	Branch string
	Old    string
	New    string
}

// Push sends one fast-forward branch update through SSH to git-receive-pack.
// Host-key verification is mandatory; hostKey must be an OpenSSH SHA256 fingerprint.
func Push(ctx context.Context, repo *gitstore.Repo, remoteURL, branch, privateKeyPEM, hostKey string, timeout time.Duration) (PushResult, error) {
	if err := validateBranch(branch); err != nil {
		return PushResult{}, err
	}
	sess, err := dialSSH(ctx, remoteURL, privateKeyPEM, hostKey, timeout)
	if err != nil {
		return PushResult{}, err
	}
	defer sess.close()

	if err := sess.start("git-receive-pack"); err != nil {
		return PushResult{}, err
	}

	reader := bufio.NewReader(sess.stdout)
	adv, err := readAdvertisement(reader)
	if err != nil {
		return PushResult{}, fmt.Errorf("读取 receive-pack 广告失败: %w", err)
	}
	refs := adv.Refs
	if !adv.Caps["report-status"] {
		return PushResult{}, errors.New("远端 receive-pack 不支持 report-status；拒绝在无法核实结果时上传")
	}

	plan, err := planPush(repo, branch, refs)
	if err != nil {
		return PushResult{}, err
	}
	if plan.UpToDate {
		_ = sess.stdin.Close()
		_ = sess.wait()
		return plan.Result(), nil
	}

	req := pushRequest{repo: repo, branch: branch, oldSHA: plan.Old, newSHA: plan.New}
	body, err := req.body()
	if err != nil {
		return PushResult{}, err
	}
	if _, err := sess.stdin.Write(body); err != nil {
		return PushResult{}, fmt.Errorf("发送 ref update 与 pack 失败: %w", err)
	}
	if err := sess.stdin.Close(); err != nil {
		return PushResult{}, fmt.Errorf("关闭 SSH stdin: %w", err)
	}

	status, err := readReportStatus(reader)
	if err != nil {
		return PushResult{}, fmt.Errorf("远端未确认 push: %w", err)
	}
	if err := checkReportStatus(status, branch); err != nil {
		return PushResult{}, err
	}
	if err := sess.wait(); err != nil {
		return PushResult{}, err
	}
	return plan.Result(), nil
}

// advertisement 是一次服务端广告的解析结果。
type advertisement struct {
	// Version 是 0 或 2。v0 里 ref 与能力混在同一个广告里，
	// v2 里广告只有能力，ref 要再发一次 ls-refs。
	Version int
	Refs    map[string]string
	Caps    map[string]bool
}

// readAdvertisement 读一次完整的服务端广告，两个协议版本都认。
//
// smart HTTP 会在广告之前先发 "# service=..." 加一个 flush；
// 那个 flush 只是分段，不是广告结束——收到它时还没见过任何内容，
// 就必须继续读，否则空仓库会被判成「远端不支持」。
func readAdvertisement(r *bufio.Reader) (advertisement, error) {
	adv := advertisement{Refs: map[string]string{}, Caps: map[string]bool{}}
	seenContent := false
	first := true
	for {
		line, kind, err := readPkt(r)
		if err != nil {
			return adv, err
		}
		if kind == pktDelim || kind == pktResponseEnd {
			continue
		}
		if kind == pktFlush {
			if !seenContent {
				continue
			}
			return adv, nil
		}
		line = strings.TrimRight(line, "\n")
		if line == "" || strings.HasPrefix(line, "# service=") {
			continue
		}
		seenContent = true

		if first && line == "version 2" {
			adv.Version = 2
			first = false
			continue
		}
		if adv.Version == 2 {
			key, value, _ := strings.Cut(line, "=")
			adv.Caps[key] = true
			if value != "" {
				adv.Caps[key+"="+value] = true
			}
			first = false
			continue
		}

		// v0：第一行带能力位（NUL 之后），之后每行是 <sha> <ref>。
		if first {
			first = false
			if i := strings.IndexByte(line, 0); i >= 0 {
				for _, c := range strings.Fields(line[i+1:]) {
					adv.Caps[c] = true
				}
				line = line[:i]
			}
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sha, ref := fields[0], fields[1]
		if !validSHA(sha) {
			continue
		}
		if ref == "capabilities^{}" {
			continue
		}
		adv.Refs[ref] = sha
	}
}

func readReportStatus(r *bufio.Reader) (map[string]string, error) {
	status := map[string]string{}
	for {
		line, kind, err := readPkt(r)
		if err != nil {
			return nil, err
		}
		if kind == pktFlush || kind == pktResponseEnd {
			break
		}
		line = strings.TrimSuffix(line, "\n")
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 {
			continue
		}
		if fields[0] == "unpack" {
			status["unpack"] = fields[1]
			continue
		}
		if fields[0] == "ok" {
			status[fields[1]] = "ok"
		} else if fields[0] == "ng" {
			parts := strings.SplitN(fields[1], " ", 2)
			if len(parts) == 2 {
				status[parts[0]] = parts[1]
			}
		}
	}
	if len(status) == 0 {
		return nil, errors.New("缺少 report-status 内容")
	}
	return status, nil
}

func pktLine(s string) []byte { return []byte(fmt.Sprintf("%04x%s", len(s)+4, s)) }

func validSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func validateBranch(branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, " \t\r\n\x00~^:?*[\\") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") || strings.Contains(branch, "//") {
		return fmt.Errorf("非法分支名 %q", branch)
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || part == "." || part == ".." || strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("非法分支名 %q", branch)
		}
	}
	return nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
