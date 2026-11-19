package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func (s *ForgeStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/healthz" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	if p == "/api/graphql" {
		s.handleGraphQL(w, r)
		return
	}
	if p == "/mcp" {
		s.handleMCP(w, r)
		return
	}
	if p == "/api/v3" || strings.HasPrefix(p, "/api/v3/") {
		s.handleREST(w, r)
		return
	}
	if s.handleGitHTTP(w, r) {
		return
	}
	s.writeJSONError(w, http.StatusNotFound, "Not Found")
}

func registerWithEnv(envURL string, port int) error {
	client := &http.Client{Timeout: 30 * time.Second}
	payload := []byte(fmt.Sprintf(`{"port":%d}`, port))
	delay := 100 * time.Millisecond
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(http.MethodPost, envURL+"/register", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
				return nil
			}
			if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusMethodNotAllowed {
				return fmt.Errorf("register failed with HTTP %d: %s", resp.StatusCode, string(body))
			}
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
		}
	}
}

func waitForDone(envURL string) {
	client := &http.Client{Timeout: 30 * time.Second}
	delay := 150 * time.Millisecond
	for {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			var body struct {
				Done bool `json:"done"`
			}
			decErr := json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decErr == nil {
				if body.Done {
					return
				}
				delay = 200 * time.Millisecond
			}
		}
		time.Sleep(delay)
		if delay < 1500*time.Millisecond {
			delay += 150 * time.Millisecond
		}
	}
}

func main() {
	store := NewForgeStore()

	listenAddr := os.Getenv("MAKEFUN_LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = "0.0.0.0:0"
	}

	lc := net.ListenConfig{KeepAlive: -1}
	ln, err := lc.Listen(context.Background(), "tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] failed to bind %s: %v\n", listenAddr, err)
		os.Exit(1)
	}
	tcpAddr := ln.Addr().(*net.TCPAddr)

	srv := newRawHTTPServer(store)

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[forge] server error: %v\n", err)
			os.Exit(1)
		}
	}()

	fmt.Fprintf(os.Stdout, "[entry] listening on port %d\n", tcpAddr.Port)

	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		// Standalone mode for local sdk/run.ts testing.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		return
	}

	if err := registerWithEnv(envURL, tcpAddr.Port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "[entry] registered; waiting for the env to say done")

	waitForDone(envURL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	fmt.Fprintln(os.Stdout, "[entry] done")
}
