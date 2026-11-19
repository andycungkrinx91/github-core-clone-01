package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func startRawTestServer(t *testing.T) (*ForgeStore, string, func()) {
	t.Helper()
	store := newTestStore(t)
	lc := net.ListenConfig{KeepAlive: -1}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := newRawHTTPServer(store)
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ln)
	}()
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil && err != http.ErrServerClosed {
			t.Errorf("serve exit: %v", err)
		}
	}
	return store, ln.Addr().String(), cleanup
}

func TestFastHTTPKeepAliveAndFallbackParity(t *testing.T) {
	_, addr, cleanup := startRawTestServer(t)
	defer cleanup()

	base := "http://" + addr
	client := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 4, DisableCompression: true},
		Timeout:   5 * time.Second,
	}
	defer client.CloseIdleConnections()

	doHTTP := func(method, path, token, body string, extra map[string]string) (*http.Response, []byte) {
		t.Helper()
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, base+path, rdr)
		if err != nil {
			t.Fatalf("new req: %v", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp, b
	}

	tok := "secret-test-token"

	// Fallback: create repo
	resp, b := doHTTP("POST", "/api/v3/user/repos", tok, `{"name":"fast-demo"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", resp.StatusCode, string(b))
	}

	// Fast path: create issue + get issue over keep-alive
	resp, b = doHTTP("POST", "/api/v3/repos/adaiversen/fast-demo/issues", tok, `{"title":"Hello","body":"World"}`, nil)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("X-Github-Request-Id") == "" {
		t.Fatalf("create issue: %d hdr=%v body=%s", resp.StatusCode, resp.Header, string(b))
	}
	resp, b = doHTTP("GET", "/api/v3/repos/adaiversen/fast-demo/issues/1", tok, "", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"Hello"`) {
		t.Fatalf("get issue: %d %s", resp.StatusCode, string(b))
	}

	// Fast path error handling: unknown label -> 422 without double-incrementing global ID
	resp, _ = doHTTP("POST", "/api/v3/repos/adaiversen/fast-demo/issues", tok, `{"title":"Bad","labels":["nope"]}`, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad label: got %d, want 422", resp.StatusCode)
	}
	resp, b = doHTTP("POST", "/api/v3/repos/adaiversen/fast-demo/issues", tok, `{"title":"Second","body":"OK"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("second issue: %d %s", resp.StatusCode, string(b))
	}
	var iss struct {
		ID     int `json:"id"`
		Number int `json:"number"`
	}
	if err := json.Unmarshal(b, &iss); err != nil || iss.Number != 2 || iss.ID != 3 {
		t.Fatalf("unexpected id/number after bad label: got %+v (want number=2, id=3)", iss)
	}

	// Fallback: malformed JSON -> 400
	resp, _ = doHTTP("POST", "/api/v3/repos/adaiversen/fast-demo/issues", tok, `{bad json`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed json: got %d, want 400", resp.StatusCode)
	}

	// Fallback: unauthenticated -> 401
	resp, _ = doHTTP("GET", "/api/v3/repos/adaiversen/fast-demo/issues/1", "", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth: got %d, want 401", resp.StatusCode)
	}

	// Fallback: unsupported method -> 405
	resp, _ = doHTTP("DELETE", "/api/v3/repos/adaiversen/fast-demo/issues/1", tok, "", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("unsupported method: got %d, want 405", resp.StatusCode)
	}

	// Fallback: 204 No Content on repo delete
	resp, b = doHTTP("DELETE", "/api/v3/repos/adaiversen/fast-demo", tok, "", nil)
	if resp.StatusCode != http.StatusNoContent || len(b) != 0 {
		t.Fatalf("delete repo: got %d len=%d", resp.StatusCode, len(b))
	}
}

func TestFastHTTPWireEdgeCases(t *testing.T) {
	_, addr, cleanup := startRawTestServer(t)
	defer cleanup()

	sendRaw := func(raw string) string {
		t.Helper()
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write([]byte(raw)); err != nil {
			t.Fatalf("write: %v", err)
		}
		data, _ := io.ReadAll(conn)
		return string(data)
	}

	// 1. Chunked transfer-encoding fallback (creates repo cleanly)
	chunkBody := `{"name":"chunked-repo"}`
	chunkedReq := "POST /api/v3/user/repos HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Authorization: Bearer secret-test-token\r\n" +
		"Content-Type: application/json\r\n" +
		"Transfer-Encoding: chunked\r\n\r\n" +
		"17\r\n" + chunkBody + "\r\n0\r\n\r\n"
	out := sendRaw(chunkedReq)
	if !strings.HasPrefix(out, "HTTP/1.1 201 Created\r\n") || !strings.Contains(out, `"chunked-repo"`) {
		t.Fatalf("chunked fallback failed: %q", out)
	}

	// 2. HTTP/1.0 closes connection by default
	http10Req := "GET /healthz HTTP/1.0\r\nHost: " + addr + "\r\n\r\n"
	out = sendRaw(http10Req)
	if !strings.HasPrefix(out, "HTTP/1.1 200 OK\r\n") || !strings.Contains(out, "Connection: close\r\n") {
		t.Fatalf("HTTP/1.0 response unexpected: %q", out)
	}

	// 3. Pipelined keep-alive requests followed by Connection: close
	pipeReq := "GET /healthz HTTP/1.1\r\nHost: " + addr + "\r\n\r\n" +
		"GET /healthz HTTP/1.1\r\nHost: " + addr + "\r\nConnection: close\r\n\r\n"
	out = sendRaw(pipeReq)
	if strings.Count(out, `{"ok":true}`) != 2 {
		t.Fatalf("pipelined keep-alive expected 2 responses, got: %q", out)
	}

	// 4. Expect: 100-continue fallback
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial 100-continue: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	expectReq := "POST /api/v3/repos/adaiversen/chunked-repo/issues HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Authorization: Bearer secret-test-token\r\n" +
		"Content-Type: application/json\r\n" +
		"Expect: 100-continue\r\n" +
		"Content-Length: 23\r\n\r\n"
	if _, err := conn.Write([]byte(expectReq)); err != nil {
		t.Fatalf("write expect headers: %v", err)
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(statusLine, "HTTP/1.1 100 Continue") {
		t.Fatalf("expected 100 Continue, got %q err=%v", statusLine, err)
	}
	_, _ = br.ReadString('\n') // blank line
	if _, err := conn.Write([]byte(`{"title":"via-100","b"}`)); err != nil {
		t.Fatalf("write expect body: %v", err)
	}
	finalLine, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(finalLine, "HTTP/1.1 400 Bad Request") {
		t.Fatalf("expected 400 after malformed 100-continue body, got %q err=%v", finalLine, err)
	}

	// 5. Malformed request line -> 400 Bad Request
	out = sendRaw("GARBAGE\r\n\r\n")
	if !strings.HasPrefix(out, "HTTP/1.1 400 Bad Request\r\n") {
		t.Fatalf("malformed request line: got %q", out)
	}

	// 6. Duplicate Content-Length fallback -> 400 Bad Request
	out = sendRaw("POST /healthz HTTP/1.1\r\nHost: x\r\nContent-Length: 1\r\nContent-Length: 2\r\n\r\na")
	if !strings.HasPrefix(out, "HTTP/1.1 400 Bad Request\r\n") {
		t.Fatalf("duplicate content-length: got %q", out)
	}

	// 7. Whitespace before colon in header name (RFC 9112 smuggling defense) -> 400 Bad Request
	out = sendRaw("GET /api/v3/repos/adaiversen/chunked-repo/issues/1 HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer secret-test-token\r\nTransfer-Encoding : chunked\r\n\r\n")
	if !strings.HasPrefix(out, "HTTP/1.1 400 Bad Request\r\n") {
		t.Fatalf("space before colon in header name: got %q", out)
	}

	// 8. Bare LF inside header line -> fallback / 400 Bad Request
	out = sendRaw("GET /healthz HTTP/1.1\r\nHost: x\r\nX-Foo: bar\nTransfer-Encoding: chunked\r\n\r\n")
	if !strings.HasPrefix(out, "HTTP/1.1 400 Bad Request\r\n") {
		t.Fatalf("bare LF smuggling: got %q", out)
	}

	// 9. HTTP/1.1 without Host header -> fallback / 400 Bad Request
	out = sendRaw("GET /healthz HTTP/1.1\r\nAuthorization: Bearer secret-test-token\r\n\r\n")
	if !strings.HasPrefix(out, "HTTP/1.1 400 Bad Request\r\n") {
		t.Fatalf("missing Host in HTTP/1.1: got %q", out)
	}

	// 10. Connection: close followed by Connection: keep-alive must still close
	out = sendRaw("GET /healthz HTTP/1.1\r\nHost: x\r\nConnection: close\r\nConnection: keep-alive\r\n\r\n")
	if !strings.HasPrefix(out, "HTTP/1.1 200 OK\r\n") || !strings.Contains(out, "Connection: close\r\n") {
		t.Fatalf("Connection: close precedence violated: got %q", out)
	}

	// 11. Duplicate Authorization header uses first value (net/http Header.Get parity)
	out = sendRaw("GET /api/v3/user HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer wrong\r\nAuthorization: Bearer secret-test-token\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(out, "HTTP/1.1 401 Unauthorized\r\n") {
		t.Fatalf("duplicate Authorization should use first header (401), got %q", out)
	}
}

func TestFastHTTPGracefulShutdownDrainsInFlightBody(t *testing.T) {
	store := newTestStore(t)
	lc := net.ListenConfig{KeepAlive: -1}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := newRawHTTPServer(store)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- srv.Serve(ln)
	}()

	addr := ln.Addr().String()
	// Create repo first.
	conn0, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial setup: %v", err)
	}
	setupBody := `{"name":"drain-repo"}`
	_, _ = conn0.Write([]byte("POST /api/v3/user/repos HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer secret-test-token\r\nContent-Type: application/json\r\nContent-Length: 21\r\nConnection: close\r\n\r\n" + setupBody))
	_, _ = io.ReadAll(conn0)
	_ = conn0.Close()

	// Start an in-flight POST /issues with headers sent, body delayed until after Shutdown begins.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial inflight: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	body := `{"title":"drained","body":"ok"}`
	hdr := "POST /api/v3/repos/adaiversen/drain-repo/issues HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Authorization: Bearer secret-test-token\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: 31\r\n\r\n"
	if _, err := conn.Write([]byte(hdr)); err != nil {
		t.Fatalf("write hdr: %v", err)
	}
	// Let server read headers and enter body-read loop.
	time.Sleep(25 * time.Millisecond)

	shutDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutDone <- srv.Shutdown(ctx)
	}()
	time.Sleep(25 * time.Millisecond)

	if _, err := conn.Write([]byte(body)); err != nil {
		t.Fatalf("write body: %v", err)
	}
	respBytes, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read drained resp: %v", err)
	}
	resp := string(respBytes)
	if !strings.HasPrefix(resp, "HTTP/1.1 201 Created\r\n") || !strings.Contains(resp, `"drained"`) {
		t.Fatalf("expected 201 Created for drained request, got %q", resp)
	}
	if err := <-shutDone; err != nil {
		t.Fatalf("shutdown returned err: %v", err)
	}
	if err := <-serveDone; err != nil && err != http.ErrServerClosed {
		t.Fatalf("serve exit err: %v", err)
	}
}

func TestFastHTTPConcurrentGitAndShutdown(t *testing.T) {
	_, addr, cleanup := startRawTestServer(t)
	defer cleanup()

	base := "http://" + addr
	tok := "secret-test-token"
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()

	req, _ := http.NewRequest("POST", base+"/api/v3/user/repos", strings.NewReader(`{"name":"git-fast"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("create git repo failed: err=%v", err)
	}
	resp.Body.Close()

	// Concurrent v2 info/refs + malformed git-receive-pack (must return 400, not 200)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := http.NewRequest("GET", base+"/adaiversen/git-fast.git/info/refs?service=git-upload-pack", nil)
			r.Header.Set("Authorization", "Bearer "+tok)
			r.Header.Set("Git-Protocol", "version=2")
			res, err := client.Do(r)
			if err != nil {
				t.Errorf("info/refs err: %v", err)
				return
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 200 || !bytes.Contains(body, []byte("version 2")) {
				t.Errorf("unexpected v2 adv: %d %q", res.StatusCode, string(body))
			}
		}()
	}
	wg.Wait()

	badPush, _ := http.NewRequest("POST", base+"/adaiversen/git-fast.git/git-receive-pack", strings.NewReader("zzzz"))
	badPush.Header.Set("Authorization", "Bearer "+tok)
	badRes, err := client.Do(badPush)
	if err != nil {
		t.Fatalf("bad push req err: %v", err)
	}
	badRes.Body.Close()
	if badRes.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed receive-pack: got %d, want 400", badRes.StatusCode)
	}
}
