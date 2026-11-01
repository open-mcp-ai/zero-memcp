package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/zero-memcp/internal/auth"
)

// 补齐未测试的 HTTP 端点：/identity /profiles /recall /api
func TestHTTPEndpoints(t *testing.T) {
	srv, dataDir := setupMountServer(t)
	tok := mintRestricted(t, dataDir, auth.Claims{Sub: "zero", Name: "boss", Cap: "rwx"})

	// 写一条普通内容，然后用 chmod rule 提升为规则，再 commit
	_, err := callMCPWithVars(t, srv, tok, "write blackegg/me@zero/rules.md -- $BODY",
		map[string]string{"BODY": "法则一：必须诚实"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callMCP(t, srv, tok, "chmod blackegg/me@zero/rules.md rule"); err != nil {
		t.Fatalf("chmod 失败: %v", err)
	}
	if _, err := callMCP(t, srv, tok, "commit blackegg/me@zero -m rules"); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/identity", srv.handleIdentity)
	mux.HandleFunc("/profiles", srv.handleProfiles)
	mux.HandleFunc("/recall", srv.handleRecall)
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/api", srv.handleAPI)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 1. /identity: 必须吐出刚才 chmod rule 的法则块
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/identity", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/identity 请求失败: %v %v", err, resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "法则一：必须诚实") {
		t.Fatalf("/identity 未包含提升后的规则内容: %s", string(body))
	}

	// 2. /profiles: 列出可见画像/分支
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/profiles", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/profiles 失败: %v %v", err, resp.Status)
	}
	var profs struct {
		Projects []map[string]any `json:"projects"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&profs)
	resp.Body.Close()
	if len(profs.Projects) == 0 {
		t.Fatal("/profiles 结果为空")
	}
	// 规则数应当反映出刚才 chmod rule 的那一条
	rules := 0
	for _, p := range profs.Projects {
		if n, ok := p["rules"].(float64); ok {
			rules += int(n)
		}
	}
	if rules == 0 {
		t.Fatalf("/profiles 未统计到规则块: %+v", profs.Projects)
	}

	// 3. /recall: POST JSON 的检索端点
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/recall", strings.NewReader(`{"text":"法则","limit":5}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/recall 失败: %v %v", err, resp.Status)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "rules.md") {
		t.Fatalf("/recall 未命中目标文件: %s", string(body))
	}

	// 4. /api?action=recent / tree
	for _, action := range []string{"recent", "tree"} {
		req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api?action="+action, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err = http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("/api?action=%s 失败: %v %v", action, err, resp.Status)
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(body) == 0 {
			t.Fatalf("/api?action=%s 返回空", action)
		}
	}
}
