package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *ForgeStore {
	t.Helper()
	t.Setenv("MAKEFUN_SHOP_TOKEN", "secret-test-token")
	t.Setenv("MAKEFUN_TRIAL_NOW", "2026-11-18T12:00:00Z")
	return NewForgeStore()
}

func doReq(s *ForgeStore, method, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://forge.local"+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr
}

func TestAuthAndOwnershipIsolation(t *testing.T) {
	s := newTestStore(t)

	// Unauthenticated across REST, GraphQL, MCP, Git
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{"GET", "/api/v3/", ""},
		{"POST", "/api/graphql", `{"query":"{ viewer { login } }"}`},
		{"POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`},
		{"GET", "/adaiversen/demo.git/info/refs?service=git-upload-pack", ""},
	} {
		rr := doReq(s, tc.method, tc.path, "", tc.body, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token: got %d, want 401", tc.method, tc.path, rr.Code)
		}
	}

	// Create repo under viewer and ensure foreign owner returns 404 (never 403).
	rr := doReq(s, "POST", "/api/v3/user/repos", "secret-test-token", `{"name":"isolated","private":true}`, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create repo: got %d (%s)", rr.Code, rr.Body.String())
	}
	foreign := doReq(s, "GET", "/api/v3/repos/stranger/isolated", "secret-test-token", "", nil)
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign owner REST: got %d, want 404", foreign.Code)
	}
	foreignGit := doReq(s, "GET", "/stranger/isolated.git/info/refs?service=git-upload-pack", "secret-test-token", "", nil)
	if foreignGit.Code != http.StatusNotFound {
		t.Fatalf("foreign owner Git: got %d, want 404", foreignGit.Code)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	s := newTestStore(t)
	doReq(s, "POST", "/api/v3/user/repos", "secret-test-token", `{"name":"safe-repo"}`, nil)

	badPaths := []string{
		"/api/v3/repos/adaiversen/safe-repo/contents/..%2Fetc%2Fpasswd",
		"/api/v3/repos/adaiversen/safe-repo/contents/a/../../secret",
	}
	for _, p := range badPaths {
		rr := doReq(s, "PUT", p, "secret-test-token", `{"message":"bad","content":"aGVsbG8="}`, nil)
		if rr.Code == http.StatusOK || rr.Code == http.StatusCreated {
			t.Fatalf("path traversal %s unexpectedly succeeded with %d", p, rr.Code)
		}
	}
}

func TestThreeWayMergeAndConflict(t *testing.T) {
	s := newTestStore(t)
	tok := "secret-test-token"
	doReq(s, "POST", "/api/v3/user/repos", tok, `{"name":"merge-repo"}`, nil)

	put := func(path, content, branch, sha string) (int, map[string]any) {
		payload := map[string]any{
			"message": "commit " + path,
			"content": base64.StdEncoding.EncodeToString([]byte(content)),
		}
		if branch != "" {
			payload["branch"] = branch
		}
		if sha != "" {
			payload["sha"] = sha
		}
		b, _ := json.Marshal(payload)
		rr := doReq(s, "PUT", "/api/v3/repos/adaiversen/merge-repo/contents/"+path, tok, string(b), nil)
		var out map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}

	code, initial := put("README.md", "base\n", "", "")
	if code != http.StatusCreated {
		t.Fatalf("initial put: %d", code)
	}
	baseCommit := initial["commit"].(map[string]any)["sha"].(string)
	baseBlob := initial["content"].(map[string]any)["sha"].(string)

	for _, br := range []string{"branch-a", "branch-b"} {
		rr := doReq(s, "POST", "/api/v3/repos/adaiversen/merge-repo/git/refs", tok,
			`{"ref":"refs/heads/`+br+`","sha":"`+baseCommit+`"}`, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create branch %s: %d", br, rr.Code)
		}
	}

	if c, _ := put("README.md", "version A\n", "branch-a", baseBlob); c != http.StatusOK {
		t.Fatalf("update on branch-a: %d", c)
	}
	if c, _ := put("README.md", "version B\n", "branch-b", baseBlob); c != http.StatusOK {
		t.Fatalf("update on branch-b: %d", c)
	}

	prA := doReq(s, "POST", "/api/v3/repos/adaiversen/merge-repo/pulls", tok,
		`{"title":"PR A","body":"","head":"branch-a","base":"main"}`, nil)
	prB := doReq(s, "POST", "/api/v3/repos/adaiversen/merge-repo/pulls", tok,
		`{"title":"PR B","body":"","head":"branch-b","base":"main"}`, nil)
	if prA.Code != http.StatusCreated || prB.Code != http.StatusCreated {
		t.Fatalf("create PRs failed: %d, %d", prA.Code, prB.Code)
	}

	mergeA := doReq(s, "PUT", "/api/v3/repos/adaiversen/merge-repo/pulls/1/merge", tok, `{"merge_method":"squash"}`, nil)
	if mergeA.Code != http.StatusOK {
		t.Fatalf("first merge should succeed: %d (%s)", mergeA.Code, mergeA.Body.String())
	}
	mergeB := doReq(s, "PUT", "/api/v3/repos/adaiversen/merge-repo/pulls/2/merge", tok, `{"merge_method":"squash"}`, nil)
	if mergeB.Code != http.StatusMethodNotAllowed {
		t.Fatalf("conflicted merge should return 405: got %d (%s)", mergeB.Code, mergeB.Body.String())
	}
}

func TestPackfileDeltaAndChecksum(t *testing.T) {
	base := []byte("hello world from git delta base line\n")
	// Delta encoding: srcLen, dstLen, copy(off=0, size=12) "hello world ", insert("forge!\n")
	var delta bytes.Buffer
	delta.WriteByte(byte(len(base)))
	delta.WriteByte(byte(12 + 7))
	delta.WriteByte(0x80 | 0x10) // copy with size byte 0 (offset=0 implied)
	delta.WriteByte(12)
	delta.WriteByte(7)
	delta.WriteString("forge!\n")

	out, err := applyGitDelta(base, delta.Bytes())
	if err != nil {
		t.Fatalf("applyGitDelta failed: %v", err)
	}
	if string(out) != "hello world forge!\n" {
		t.Fatalf("unexpected delta result: %q", string(out))
	}

	// Verify corrupted pack checksum is rejected.
	repo := &Repository{Objects: make(map[string]GitObject)}
	pack := buildPackfile([]GitObject{{Type: "blob", Data: []byte("abc")}})
	pack[len(pack)-1] ^= 0xff
	if err := unpackPackfileLocked(repo, pack); err == nil {
		t.Fatalf("expected checksum mismatch error on corrupted pack")
	}
}

func TestConcurrentIssuesAndMCP(t *testing.T) {
	s := newTestStore(t)
	tok := "secret-test-token"
	doReq(s, "POST", "/api/v3/user/repos", tok, `{"name":"conc"}`, nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := strconv.Itoa(idx)
			rr := doReq(s, "POST", "/api/v3/repos/adaiversen/conc/issues", tok,
				`{"title":"issue `+id+`","body":"body"}`, nil)
			if rr.Code != http.StatusCreated {
				t.Errorf("issue create %d failed: %d", idx, rr.Code)
			}
			mcpBody := `{"jsonrpc":"2.0","id":"req-` + id + `","method":"tools/call","params":{"name":"get_repo","arguments":{"owner":"adaiversen","repo":"conc"}}}`
			mrr := doReq(s, "POST", "/mcp", tok, mcpBody, nil)
			if mrr.Code != http.StatusOK {
				t.Errorf("mcp call %d failed: %d", idx, mrr.Code)
			}
			var rpc struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(mrr.Body.Bytes(), &rpc); err != nil || rpc.ID != "req-"+id {
				t.Errorf("mcp id mismatch: got %q, want req-%s", rpc.ID, id)
			}
		}(i)
	}
	wg.Wait()
}
