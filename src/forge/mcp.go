package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var mcpToolsList = []mcpToolDef{
	{
		Name:        "create_repo",
		Description: "Create a new repository for the authenticated user.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "Repository name"},
				"description": map[string]any{"type": "string", "description": "Repository description"},
				"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Fetch metadata for a repository by owner and repository name.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner login"},
				"repo":  map[string]any{"type": "string", "description": "Repository name"},
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create a new issue in a repository.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner login"},
				"repo":  map[string]any{"type": "string", "description": "Repository name"},
				"title": map[string]any{"type": "string", "description": "Issue title"},
				"body":  map[string]any{"type": "string", "description": "Issue body"},
			},
			"required": []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository filtered by state.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner login"},
				"repo":  map[string]any{"type": "string", "description": "Repository name"},
				"state": map[string]any{"type": "string", "description": "Filter by issue state: open, closed, or all"},
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Open a new pull request from head branch into base branch.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner login"},
				"repo":  map[string]any{"type": "string", "description": "Repository name"},
				"title": map[string]any{"type": "string", "description": "Pull request title"},
				"body":  map[string]any{"type": "string", "description": "Pull request body"},
				"head":  map[string]any{"type": "string", "description": "Head branch name"},
				"base":  map[string]any{"type": "string", "description": "Base branch name"},
			},
			"required": []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge an open pull request into its base branch.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":       map[string]any{"type": "string", "description": "Repository owner login"},
				"repo":        map[string]any{"type": "string", "description": "Repository name"},
				"pull_number": map[string]any{"type": "integer", "description": "Pull request number"},
			},
			"required": []string{"owner", "repo", "pull_number"},
		},
	},
}

var mcpToolsResultRaw = func() json.RawMessage {
	b, _ := json.Marshal(map[string]any{"tools": mcpToolsList})
	return json.RawMessage(b)
}()

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeRPCResult(w http.ResponseWriter, status int, id json.RawMessage, result any) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	b, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   jsonRPCError{Code: code, Message: msg},
	}
	b, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func mcpToolSuccess(payload any) map[string]any {
	b, _ := json.Marshal(payload)
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(b)},
		},
		"structuredContent": payload,
		"isError":           false,
	}
}

func mcpToolFailure(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": msg},
		},
		"isError": true,
	}
}

func (s *ForgeStore) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !s.CheckAuth(r.Header.Get("Authorization")) {
		s.writeJSONError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBodyBytes))
	if err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, -32700, "Parse error")
		return
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, -32700, "Parse error")
		return
	}

	// Notifications have no id.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}

	switch req.Method {
	case "server/discover":
		serverInfo := map[string]string{
			"name":    "makefun-git-forge",
			"version": "1.0.0",
		}
		writeRPCResult(w, http.StatusOK, req.ID, map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18"},
			"serverInfo":        serverInfo,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": serverInfo,
			},
		})
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		ver := params.ProtocolVersion
		if ver == "" {
			ver = "2025-11-25"
		}
		writeRPCResult(w, http.StatusOK, req.ID, map[string]any{
			"protocolVersion": ver,
			"serverInfo": map[string]string{
				"name":    "makefun-git-forge",
				"version": "1.0.0",
			},
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
		})
	case "tools/list":
		writeRPCResult(w, http.StatusOK, req.ID, mcpToolsResultRaw)
	case "tools/call":
		s.handleMCPToolCall(w, req)
	default:
		writeRPCError(w, http.StatusOK, req.ID, -32601, "Method not found: "+req.Method)
	}
}

func stringArg(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	str, ok := v.(string)
	return str, ok && str != ""
}

func intArg(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), int(n) > 0
	case int:
		return n, n > 0
	default:
		return 0, false
	}
}

func (s *ForgeStore) handleMCPToolCall(w http.ResponseWriter, req jsonRPCRequest) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		writeRPCError(w, http.StatusOK, req.ID, -32602, "Invalid params")
		return
	}
	args := params.Arguments
	if args == nil {
		args = map[string]any{}
	}

	switch params.Name {
	case "create_repo":
		name, ok := stringArg(args, "name")
		if !ok || !isSafeName(name) {
			writeRPCError(w, http.StatusOK, req.ID, -32602, "Missing required argument: name")
			return
		}
		desc, _ := args["description"].(string)
		priv, _ := args["private"].(bool)
		repo, created := s.CreateRepo(viewerLogin, name, desc, priv)
		if !created {
			writeRPCResult(w, http.StatusOK, req.ID, mcpToolFailure("Repository already exists"))
			return
		}
		repo.mu.RLock()
		out := repo.toJSONLocked()
		repo.mu.RUnlock()
		writeRPCResult(w, http.StatusOK, req.ID, mcpToolSuccess(out))

	case "get_repo":
		owner, okO := stringArg(args, "owner")
		repoName, okR := stringArg(args, "repo")
		if !okO || !okR {
			writeRPCError(w, http.StatusOK, req.ID, -32602, "Missing required arguments: owner, repo")
			return
		}
		repo := s.GetRepo(owner, repoName)
		if repo == nil || owner != viewerLogin {
			writeRPCResult(w, http.StatusOK, req.ID, mcpToolFailure("Repository not found"))
			return
		}
		repo.mu.RLock()
		out := repo.toJSONLocked()
		repo.mu.RUnlock()
		writeRPCResult(w, http.StatusOK, req.ID, mcpToolSuccess(out))

	case "create_issue":
		owner, okO := stringArg(args, "owner")
		repoName, okR := stringArg(args, "repo")
		title, okT := stringArg(args, "title")
		if !okO || !okR || !okT {
			writeRPCError(w, http.StatusOK, req.ID, -32602, "Missing required arguments: owner, repo, title")
			return
		}
		body, _ := args["body"].(string)
		repo := s.GetRepo(owner, repoName)
		if repo == nil || owner != viewerLogin {
			writeRPCResult(w, http.StatusOK, req.ID, mcpToolFailure("Repository not found"))
			return
		}
		issue, err := s.CreateIssue(repo, title, body, nil)
		if err != nil {
			writeRPCResult(w, http.StatusOK, req.ID, mcpToolFailure(err.Error()))
			return
		}
		writeRPCResult(w, http.StatusOK, req.ID, mcpToolSuccess(issue))

	case "list_issues":
		owner, okO := stringArg(args, "owner")
		repoName, okR := stringArg(args, "repo")
		if !okO || !okR {
			writeRPCError(w, http.StatusOK, req.ID, -32602, "Missing required arguments: owner, repo")
			return
		}
		state, _ := args["state"].(string)
		if state == "" {
			state = "open"
		}
		repo := s.GetRepo(owner, repoName)
		if repo == nil || owner != viewerLogin {
			writeRPCResult(w, http.StatusOK, req.ID, mcpToolFailure("Repository not found"))
			return
		}
		repo.mu.RLock()
		out := make([]*Issue, 0, len(repo.IssueOrder))
		for i := len(repo.IssueOrder) - 1; i >= 0; i-- {
			iss := repo.Issues[repo.IssueOrder[i]]
			if state == "all" || iss.State == state {
				out = append(out, iss)
			}
		}
		repo.mu.RUnlock()
		writeRPCResult(w, http.StatusOK, req.ID, mcpToolSuccess(out))

	case "create_pull_request":
		owner, okO := stringArg(args, "owner")
		repoName, okR := stringArg(args, "repo")
		title, okT := stringArg(args, "title")
		head, okH := stringArg(args, "head")
		base, okB := stringArg(args, "base")
		if !okO || !okR || !okT || !okH || !okB {
			writeRPCError(w, http.StatusOK, req.ID, -32602, "Missing required arguments: owner, repo, title, head, base")
			return
		}
		body, _ := args["body"].(string)
		repo := s.GetRepo(owner, repoName)
		if repo == nil || owner != viewerLogin {
			writeRPCResult(w, http.StatusOK, req.ID, mcpToolFailure("Repository not found"))
			return
		}
		pr, err := s.CreatePull(repo, title, body, head, base)
		if err != nil {
			writeRPCResult(w, http.StatusOK, req.ID, mcpToolFailure(err.Error()))
			return
		}
		writeRPCResult(w, http.StatusOK, req.ID, mcpToolSuccess(pr))

	case "merge_pull_request":
		owner, okO := stringArg(args, "owner")
		repoName, okR := stringArg(args, "repo")
		pullNum, okN := intArg(args, "pull_number")
		if !okO || !okR || !okN {
			writeRPCError(w, http.StatusOK, req.ID, -32602, "Missing required arguments: owner, repo, pull_number")
			return
		}
		repo := s.GetRepo(owner, repoName)
		if repo == nil || owner != viewerLogin {
			writeRPCResult(w, http.StatusOK, req.ID, mcpToolFailure("Repository not found"))
			return
		}
		res, _, err := s.MergePull(repo, pullNum, "")
		if err != nil {
			writeRPCResult(w, http.StatusOK, req.ID, mcpToolFailure(fmt.Sprintf("merge failed: %v", err)))
			return
		}
		writeRPCResult(w, http.StatusOK, req.ID, mcpToolSuccess(res))

	default:
		writeRPCError(w, http.StatusOK, req.ID, -32601, "Unknown tool: "+params.Name)
	}
}
