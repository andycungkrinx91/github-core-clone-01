package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
)

const maxJSONBodyBytes = 8 << 20

var jsonContentTypeHdr = []string{"application/json; charset=utf-8"}

func (s *ForgeStore) writeJSONBytes(w http.ResponseWriter, status int, payload []byte) {
	h := w.Header()
	h["Content-Type"] = jsonContentTypeHdr
	h["X-Github-Request-Id"] = []string{s.NextRequestID()}
	h["Date"] = nil
	if status != http.StatusNoContent {
		h["Content-Length"] = []string{strconv.Itoa(len(payload))}
	}
	w.WriteHeader(status)
	if status != http.StatusNoContent && len(payload) > 0 {
		_, _ = w.Write(payload)
	}
}

func (s *ForgeStore) writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "internal serialization error")
		return
	}
	s.writeJSONBytes(w, status, b)
}

func (s *ForgeStore) writeJSONError(w http.ResponseWriter, status int, msg string) {
	b, _ := json.Marshal(map[string]string{"message": msg})
	s.writeJSONBytes(w, status, b)
}

func decodeJSONBody(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBodyBytes))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	return json.Unmarshal(body, dst)
}

func isSafeName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 200 {
		return false
	}
	return !strings.ContainsAny(name, "/\\\x00")
}

func cleanRepoRelPath(raw string) (string, bool) {
	p := strings.TrimPrefix(raw, "/")
	if p == "" || strings.ContainsRune(p, 0) || strings.ContainsRune(p, '\\') {
		return "", false
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
		out = append(out, seg)
	}
	return strings.Join(out, "/"), true
}

func parsePageParams(r *http.Request, defaultPerPage int) (int, int) {
	q := r.URL.Query()
	perPage := defaultPerPage
	if v := q.Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			perPage = n
		}
	}
	page := 1
	if v := q.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	return page, perPage
}

func (s *ForgeStore) handleREST(w http.ResponseWriter, r *http.Request) {
	if !s.CheckAuth(r.Header.Get("Authorization")) {
		s.writeJSONError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	sub := strings.TrimPrefix(r.URL.Path, "/api/v3")
	if sub == "" || sub == "/" {
		if r.Method != http.MethodGet {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		s.writeJSON(w, http.StatusOK, map[string]string{
			"current_user_url": "/api/v3/user",
			"repository_url":   "/api/v3/repos/{owner}/{repo}",
		})
		return
	}

	switch sub {
	case "/meta":
		if r.Method != http.MethodGet {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"installed_version":                  "1.0.0",
			"verifiable_password_authentication": true,
		})
		return
	case "/user":
		if r.Method != http.MethodGet {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		s.writeJSON(w, http.StatusOK, User{ID: viewerID, Login: viewerLogin, Name: viewerName})
		return
	case "/user/repos":
		switch r.Method {
		case http.MethodGet:
			s.handleListUserRepos(w, r)
		case http.MethodPost:
			s.handleCreateUserRepo(w, r)
		default:
			s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
		return
	}

	if strings.HasPrefix(sub, "/repos/") {
		s.handleReposSubroute(w, r, strings.TrimPrefix(sub, "/repos/"))
		return
	}

	s.writeJSONError(w, http.StatusNotFound, "Not Found")
}

func (s *ForgeStore) handleListUserRepos(w http.ResponseWriter, r *http.Request) {
	repos := s.ListViewerRepos()
	page, perPage := parsePageParams(r, 30)
	start := (page - 1) * perPage
	if start >= len(repos) {
		s.writeJSON(w, http.StatusOK, []RepoJSON{})
		return
	}
	end := start + perPage
	if end > len(repos) {
		end = len(repos)
	}
	if end < len(repos) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		nextURL := fmt.Sprintf("%s://%s/api/v3/user/repos?per_page=%d&page=%d", scheme, r.Host, perPage, page+1)
		w.Header().Set("Link", fmt.Sprintf("<%s>; rel=\"next\"", nextURL))
	}
	s.writeJSON(w, http.StatusOK, repos[start:end])
}

func (s *ForgeStore) handleCreateUserRepo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Private     bool   `json:"private"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if !isSafeName(req.Name) {
		s.writeJSONError(w, http.StatusUnprocessableEntity, "Invalid repository name")
		return
	}
	repo, created := s.CreateRepo(viewerLogin, req.Name, req.Description, req.Private)
	if !created {
		s.writeJSONError(w, http.StatusUnprocessableEntity, "Repository creation failed: name already exists on this account")
		return
	}
	repo.mu.RLock()
	out := repo.toJSONLocked()
	repo.mu.RUnlock()
	s.writeJSON(w, http.StatusCreated, out)
}

func (s *ForgeStore) handleReposSubroute(w http.ResponseWriter, r *http.Request, tail string) {
	parts := strings.SplitN(tail, "/", 3)
	if len(parts) < 2 || !isSafeName(parts[0]) || !isSafeName(parts[1]) {
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	owner, repoName := parts[0], parts[1]
	rest := ""
	if len(parts) == 3 {
		rest = parts[2]
	}

	// Enforce ownership: token belongs to viewerLogin; any repo under another owner is 404.
	if owner != viewerLogin {
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	repo := s.GetRepo(owner, repoName)
	if repo == nil {
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
		return
	}

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			repo.mu.RLock()
			out := repo.toJSONLocked()
			repo.mu.RUnlock()
			s.writeJSON(w, http.StatusOK, out)
		case http.MethodPatch:
			var req struct {
				Description *string `json:"description"`
				Private     *bool   `json:"private"`
			}
			if err := decodeJSONBody(r, &req); err != nil {
				s.writeJSONError(w, http.StatusBadRequest, "Problems parsing JSON")
				return
			}
			repo.mu.Lock()
			if req.Description != nil {
				repo.Description = *req.Description
			}
			if req.Private != nil {
				repo.Private = *req.Private
			}
			out := repo.toJSONLocked()
			repo.mu.Unlock()
			s.writeJSON(w, http.StatusOK, out)
		case http.MethodDelete:
			s.DeleteRepo(owner, repoName)
			s.writeJSONBytes(w, http.StatusNoContent, nil)
		default:
			s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
		return
	}

	// Fast path for hot issue reads/writes.
	if strings.HasPrefix(rest, "issues/") {
		s.handleIssueItemRoutes(w, r, repo, strings.TrimPrefix(rest, "issues/"))
		return
	}
	if rest == "issues" {
		s.handleIssuesCollection(w, r, repo)
		return
	}

	switch {
	case rest == "git/refs":
		s.handleCreateGitRef(w, r, repo)
	case strings.HasPrefix(rest, "git/refs/"):
		s.handleGetGitRef(w, r, repo, strings.TrimPrefix(rest, "git/refs/"))
	case rest == "branches":
		s.handleListBranches(w, r, repo)
	case strings.HasPrefix(rest, "branches/"):
		s.handleGetBranch(w, r, repo, strings.TrimPrefix(rest, "branches/"))
	case rest == "commits":
		s.handleListCommits(w, r, repo)
	case strings.HasPrefix(rest, "contents/"):
		s.handleContents(w, r, repo, strings.TrimPrefix(rest, "contents/"))
	case rest == "contents":
		s.handleContents(w, r, repo, "")
	case rest == "labels":
		s.handleLabels(w, r, repo)
	case rest == "pulls":
		s.handlePullsCollection(w, r, repo)
	case strings.HasPrefix(rest, "pulls/"):
		s.handlePullItemRoutes(w, r, repo, strings.TrimPrefix(rest, "pulls/"))
	default:
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
	}
}

func (s *ForgeStore) handleCreateGitRef(w http.ResponseWriter, r *http.Request, repo *Repository) {
	if r.Method != http.MethodPost {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	var req struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if req.Ref == "" || len(req.SHA) != 40 {
		s.writeJSONError(w, http.StatusUnprocessableEntity, "Invalid ref or sha")
		return
	}
	ref := req.Ref
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/" + strings.TrimPrefix(ref, "/")
	}

	repo.mu.Lock()
	if _, exists := repo.Refs[ref]; exists {
		repo.mu.Unlock()
		s.writeJSONError(w, http.StatusUnprocessableEntity, "Reference already exists")
		return
	}
	repo.Refs[ref] = req.SHA
	repo.invalidateGitCachesLocked()
	repo.mu.Unlock()

	s.writeJSON(w, http.StatusCreated, map[string]any{
		"ref": ref,
		"object": map[string]string{
			"sha":  req.SHA,
			"type": "commit",
		},
	})
}

func (s *ForgeStore) handleGetGitRef(w http.ResponseWriter, r *http.Request, repo *Repository, refTail string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	fullRef := "refs/" + refTail
	repo.mu.RLock()
	sha, ok := repo.Refs[fullRef]
	repo.mu.RUnlock()
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ref": fullRef,
		"object": map[string]string{
			"sha":  sha,
			"type": "commit",
		},
	})
}

func (s *ForgeStore) handleListBranches(w http.ResponseWriter, r *http.Request, repo *Repository) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	type branchCommit struct {
		SHA string `json:"sha"`
	}
	type branchItem struct {
		Name   string       `json:"name"`
		Commit branchCommit `json:"commit"`
	}
	repo.mu.RLock()
	out := make([]branchItem, 0, len(repo.Refs))
	for _, ref := range repo.sortedRefNamesLocked() {
		if strings.HasPrefix(ref, "refs/heads/") {
			out = append(out, branchItem{
				Name:   strings.TrimPrefix(ref, "refs/heads/"),
				Commit: branchCommit{SHA: repo.Refs[ref]},
			})
		}
	}
	repo.mu.RUnlock()
	s.writeJSON(w, http.StatusOK, out)
}

func (s *ForgeStore) handleGetBranch(w http.ResponseWriter, r *http.Request, repo *Repository, branch string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	repo.mu.RLock()
	sha, ok := repo.Refs["refs/heads/"+branch]
	repo.mu.RUnlock()
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "Branch not found")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"name":   branch,
		"commit": map[string]string{"sha": sha},
	})
}

func (s *ForgeStore) resolveRefLocked(repo *Repository, ref string) (string, bool) {
	if ref == "" {
		ref = repo.DefaultBranch
	}
	if sha, ok := repo.Refs["refs/heads/"+ref]; ok {
		return sha, true
	}
	if sha, ok := repo.Refs[ref]; ok {
		return sha, true
	}
	if sha, ok := repo.Refs["refs/tags/"+ref]; ok {
		if peeled, okP := repo.peelTagLocked(sha); okP {
			return peeled, true
		}
		return sha, true
	}
	if obj, ok := repo.Objects[ref]; ok && obj.Type == "commit" {
		return ref, true
	}
	return "", false
}

func (s *ForgeStore) handleListCommits(w http.ResponseWriter, r *http.Request, repo *Repository) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	shaParam := r.URL.Query().Get("sha")
	type commitDetail struct {
		Message string `json:"message"`
	}
	type commitItem struct {
		SHA    string       `json:"sha"`
		Commit commitDetail `json:"commit"`
	}

	repo.mu.RLock()
	tip, ok := s.resolveRefLocked(repo, shaParam)
	if !ok {
		repo.mu.RUnlock()
		s.writeJSON(w, http.StatusOK, []commitItem{})
		return
	}
	out := make([]commitItem, 0, 16)
	visited := make(map[string]struct{}, 16)
	queue := []string{tip}
	visited[tip] = struct{}{}
	for len(queue) > 0 && len(out) < 100 {
		cur := queue[0]
		queue = queue[1:]
		obj, exists := repo.Objects[cur]
		if !exists || obj.Type != "commit" {
			continue
		}
		parsed, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		out = append(out, commitItem{
			SHA:    cur,
			Commit: commitDetail{Message: strings.TrimSuffix(parsed.Message, "\n")},
		})
		for _, p := range parsed.Parents {
			if _, seen := visited[p]; !seen {
				visited[p] = struct{}{}
				queue = append(queue, p)
			}
		}
	}
	repo.mu.RUnlock()
	s.writeJSON(w, http.StatusOK, out)
}

func (s *ForgeStore) handleContents(w http.ResponseWriter, r *http.Request, repo *Repository, rawPath string) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetContents(w, r, repo, rawPath)
	case http.MethodPut:
		s.handlePutContents(w, r, repo, rawPath)
	case http.MethodDelete:
		s.handleDeleteContents(w, r, repo, rawPath)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

func (s *ForgeStore) handleGetContents(w http.ResponseWriter, r *http.Request, repo *Repository, rawPath string) {
	refParam := r.URL.Query().Get("ref")
	cleanPath := ""
	if rawPath != "" {
		var ok bool
		cleanPath, ok = cleanRepoRelPath(rawPath)
		if !ok {
			s.writeJSONError(w, http.StatusNotFound, "Not Found")
			return
		}
	}

	repo.mu.RLock()
	defer repo.mu.RUnlock()

	tip, ok := s.resolveRefLocked(repo, refParam)
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	commitObj, ok := repo.Objects[tip]
	if !ok || commitObj.Type != "commit" {
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	parsedCommit, err := parseCommit(commitObj.Data)
	if err != nil {
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
		return
	}

	curOID := parsedCommit.Tree
	isDir := true
	if cleanPath != "" {
		segments := strings.Split(cleanPath, "/")
		for idx, seg := range segments {
			treeObj, exists := repo.Objects[curOID]
			if !exists || treeObj.Type != "tree" {
				s.writeJSONError(w, http.StatusNotFound, "Not Found")
				return
			}
			entries, err := parseTree(treeObj.Data)
			if err != nil {
				s.writeJSONError(w, http.StatusNotFound, "Not Found")
				return
			}
			found := false
			for _, e := range entries {
				if e.Name == seg {
					curOID = e.OID
					isDir = (e.Mode == "40000" || e.Mode == "040000")
					found = true
					break
				}
			}
			if !found {
				s.writeJSONError(w, http.StatusNotFound, "Not Found")
				return
			}
			if idx < len(segments)-1 && !isDir {
				s.writeJSONError(w, http.StatusNotFound, "Not Found")
				return
			}
		}
	}

	if isDir {
		treeObj, exists := repo.Objects[curOID]
		if !exists || treeObj.Type != "tree" {
			s.writeJSONError(w, http.StatusNotFound, "Not Found")
			return
		}
		entries, err := parseTree(treeObj.Data)
		if err != nil {
			s.writeJSONError(w, http.StatusNotFound, "Not Found")
			return
		}
		type dirEntry struct {
			Type string `json:"type"`
			Name string `json:"name"`
			Path string `json:"path"`
			SHA  string `json:"sha"`
			Size int    `json:"size"`
		}
		out := make([]dirEntry, 0, len(entries))
		for _, e := range entries {
			t := "file"
			sz := 0
			if e.Mode == "40000" || e.Mode == "040000" {
				t = "dir"
			} else if bObj, ok := repo.Objects[e.OID]; ok {
				sz = len(bObj.Data)
			}
			ep := e.Name
			if cleanPath != "" {
				ep = cleanPath + "/" + e.Name
			}
			out = append(out, dirEntry{
				Type: t,
				Name: e.Name,
				Path: ep,
				SHA:  e.OID,
				Size: sz,
			})
		}
		s.writeJSON(w, http.StatusOK, out)
		return
	}

	blobObj, exists := repo.Objects[curOID]
	if !exists || blobObj.Type != "blob" {
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"type":     "file",
		"name":     path.Base(cleanPath),
		"path":     cleanPath,
		"sha":      curOID,
		"size":     len(blobObj.Data),
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString(blobObj.Data),
	})
}

func (s *ForgeStore) handlePutContents(w http.ResponseWriter, r *http.Request, repo *Repository, rawPath string) {
	cleanPath, ok := cleanRepoRelPath(rawPath)
	if !ok {
		s.writeJSONError(w, http.StatusUnprocessableEntity, "Invalid path")
		return
	}
	var req struct {
		Message string `json:"message"`
		Content string `json:"content"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	cleanedB64 := strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(req.Content)
	rawBytes, err := base64.StdEncoding.DecodeString(cleanedB64)
	if err != nil {
		s.writeJSONError(w, http.StatusUnprocessableEntity, "content is not valid base64")
		return
	}

	repo.mu.Lock()
	branch := req.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	refName := "refs/heads/" + branch
	parentOID, hasBranch := repo.Refs[refName]
	if !hasBranch && branch != repo.DefaultBranch {
		// Allow creating branch off default branch if default branch exists.
		if defOID, okDef := repo.Refs["refs/heads/"+repo.DefaultBranch]; okDef {
			parentOID = defOID
			hasBranch = true
		}
	}

	files := make(map[string]string)
	if hasBranch {
		files = repo.flattenCommitFilesLocked(parentOID)
	}

	existingSHA, exists := files[cleanPath]
	status := http.StatusCreated
	if exists {
		if req.SHA == "" || !strings.EqualFold(req.SHA, existingSHA) {
			repo.mu.Unlock()
			s.writeJSONError(w, http.StatusConflict, "sha does not match")
			return
		}
		status = http.StatusOK
	} else if req.SHA != "" {
		repo.mu.Unlock()
		s.writeJSONError(w, http.StatusConflict, "sha supplied for non-existent file")
		return
	}

	blobOID := repo.putObjectLocked(GitObject{Type: "blob", Data: rawBytes})
	files[cleanPath] = blobOID
	treeOID := repo.writeFilesTreeLocked(files)

	var parents []string
	if hasBranch && parentOID != "" {
		parents = []string{parentOID}
	}
	msg := req.Message
	if msg == "" {
		msg = "Update " + cleanPath
	}
	commitMsg := msg
	if !strings.HasSuffix(commitMsg, "\n") {
		commitMsg += "\n"
	}
	repo.CommitSeq++
	ts := s.trialEpoch + repo.CommitSeq
	commitBytes := buildCommitBytes(treeOID, parents, viewerName+" <"+viewerEmail+">", ts, commitMsg)
	commitOID := repo.putObjectLocked(GitObject{Type: "commit", Data: commitBytes})
	repo.Refs[refName] = commitOID
	repo.invalidateGitCachesLocked()
	repo.mu.Unlock()

	s.writeJSON(w, status, map[string]any{
		"content": map[string]any{
			"name":     path.Base(cleanPath),
			"path":     cleanPath,
			"sha":      blobOID,
			"size":     len(rawBytes),
			"type":     "file",
			"encoding": "base64",
		},
		"commit": map[string]any{
			"sha":     commitOID,
			"message": msg,
		},
	})
}

func (s *ForgeStore) handleDeleteContents(w http.ResponseWriter, r *http.Request, repo *Repository, rawPath string) {
	cleanPath, ok := cleanRepoRelPath(rawPath)
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Message string `json:"message"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}

	repo.mu.Lock()
	branch := req.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	refName := "refs/heads/" + branch
	parentOID, hasBranch := repo.Refs[refName]
	if !hasBranch {
		repo.mu.Unlock()
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	files := repo.flattenCommitFilesLocked(parentOID)
	existingSHA, exists := files[cleanPath]
	if !exists {
		repo.mu.Unlock()
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	if req.SHA != "" && !strings.EqualFold(req.SHA, existingSHA) {
		repo.mu.Unlock()
		s.writeJSONError(w, http.StatusConflict, "sha does not match")
		return
	}

	delete(files, cleanPath)
	treeOID := repo.writeFilesTreeLocked(files)
	msg := req.Message
	if msg == "" {
		msg = "Delete " + cleanPath
	}
	commitMsg := msg
	if !strings.HasSuffix(commitMsg, "\n") {
		commitMsg += "\n"
	}
	repo.CommitSeq++
	ts := s.trialEpoch + repo.CommitSeq
	commitBytes := buildCommitBytes(treeOID, []string{parentOID}, viewerName+" <"+viewerEmail+">", ts, commitMsg)
	commitOID := repo.putObjectLocked(GitObject{Type: "commit", Data: commitBytes})
	repo.Refs[refName] = commitOID
	repo.invalidateGitCachesLocked()
	repo.mu.Unlock()

	s.writeJSON(w, http.StatusOK, map[string]any{
		"content": nil,
		"commit": map[string]any{
			"sha":     commitOID,
			"message": msg,
		},
	})
}

func (s *ForgeStore) handleLabels(w http.ResponseWriter, r *http.Request, repo *Repository) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}
		if err := decodeJSONBody(r, &req); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		if req.Name == "" {
			s.writeJSONError(w, http.StatusUnprocessableEntity, "Label name is required")
			return
		}
		color := req.Color
		if color == "" {
			color = "ededed"
		}
		repo.mu.Lock()
		lbl := Label{ID: repo.NextLabelID, Name: req.Name, Color: color}
		repo.NextLabelID++
		repo.Labels[req.Name] = lbl
		repo.mu.Unlock()
		s.writeJSON(w, http.StatusCreated, lbl)
	case http.MethodGet:
		repo.mu.RLock()
		names := make([]string, 0, len(repo.Labels))
		for k := range repo.Labels {
			names = append(names, k)
		}
		sort.Strings(names)
		out := make([]Label, 0, len(names))
		for _, k := range names {
			out = append(out, repo.Labels[k])
		}
		repo.mu.RUnlock()
		s.writeJSON(w, http.StatusOK, out)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

func (s *ForgeStore) handleIssuesCollection(w http.ResponseWriter, r *http.Request, repo *Repository) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		if err := decodeJSONBody(r, &req); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		issue, err := s.CreateIssue(repo, req.Title, req.Body, req.Labels)
		if err != nil {
			s.writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		s.writeJSONBytes(w, http.StatusCreated, issue.cachedJSON)
	case http.MethodGet:
		q := r.URL.Query()
		state := q.Get("state")
		if state == "" {
			state = "open"
		}
		direction := q.Get("direction")
		if direction == "" {
			direction = "desc"
		}
		page, perPage := parsePageParams(r, 30)

		repo.mu.RLock()
		filtered := make([]*Issue, 0, len(repo.IssueOrder))
		if direction == "asc" {
			for _, num := range repo.IssueOrder {
				iss := repo.Issues[num]
				if state == "all" || iss.State == state {
					filtered = append(filtered, iss)
				}
			}
		} else {
			for i := len(repo.IssueOrder) - 1; i >= 0; i-- {
				iss := repo.Issues[repo.IssueOrder[i]]
				if state == "all" || iss.State == state {
					filtered = append(filtered, iss)
				}
			}
		}
		start := (page - 1) * perPage
		if start >= len(filtered) {
			repo.mu.RUnlock()
			s.writeJSONBytes(w, http.StatusOK, []byte("[]"))
			return
		}
		end := start + perPage
		if end > len(filtered) {
			end = len(filtered)
		}
		// Assemble JSON array directly from pre-serialized cachedJSON buffers.
		totalBytes := 2
		for _, iss := range filtered[start:end] {
			totalBytes += len(iss.cachedJSON) + 1
		}
		buf := make([]byte, 0, totalBytes)
		buf = append(buf, '[')
		for idx, iss := range filtered[start:end] {
			if idx > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, iss.cachedJSON...)
		}
		buf = append(buf, ']')
		repo.mu.RUnlock()
		s.writeJSONBytes(w, http.StatusOK, buf)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

func (s *ForgeStore) handleIssueItemRoutes(w http.ResponseWriter, r *http.Request, repo *Repository, tail string) {
	parts := strings.SplitN(tail, "/", 2)
	num, err := strconv.Atoi(parts[0])
	if err != nil || num <= 0 {
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			repo.mu.RLock()
			iss, ok := repo.Issues[num]
			var payload []byte
			if ok {
				payload = iss.cachedJSON
			}
			repo.mu.RUnlock()
			if !ok {
				s.writeJSONError(w, http.StatusNotFound, "Not Found")
				return
			}
			s.writeJSONBytes(w, http.StatusOK, payload)
		case http.MethodPatch:
			var req struct {
				State *string `json:"state"`
				Title *string `json:"title"`
				Body  *string `json:"body"`
			}
			if err := decodeJSONBody(r, &req); err != nil {
				s.writeJSONError(w, http.StatusBadRequest, "Problems parsing JSON")
				return
			}
			repo.mu.Lock()
			iss, ok := repo.Issues[num]
			if !ok {
				repo.mu.Unlock()
				s.writeJSONError(w, http.StatusNotFound, "Not Found")
				return
			}
			if req.State != nil && (*req.State == "open" || *req.State == "closed") {
				iss.State = *req.State
			}
			if req.Title != nil {
				iss.Title = *req.Title
			}
			if req.Body != nil {
				iss.Body = *req.Body
			}
			iss.rebuildJSONLocked()
			payload := iss.cachedJSON
			repo.mu.Unlock()
			s.writeJSONBytes(w, http.StatusOK, payload)
		default:
			s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
		return
	}

	if parts[1] == "comments" {
		switch r.Method {
		case http.MethodPost:
			var req struct {
				Body string `json:"body"`
			}
			if err := decodeJSONBody(r, &req); err != nil {
				s.writeJSONError(w, http.StatusBadRequest, "Problems parsing JSON")
				return
			}
			c, ok := s.AddComment(repo, num, req.Body)
			if !ok {
				s.writeJSONError(w, http.StatusNotFound, "Not Found")
				return
			}
			s.writeJSON(w, http.StatusCreated, c)
		case http.MethodGet:
			repo.mu.RLock()
			iss, ok := repo.Issues[num]
			if !ok {
				repo.mu.RUnlock()
				s.writeJSONError(w, http.StatusNotFound, "Not Found")
				return
			}
			out := make([]Comment, 0, len(iss.Comments))
			for _, c := range iss.Comments {
				out = append(out, *c)
			}
			repo.mu.RUnlock()
			s.writeJSON(w, http.StatusOK, out)
		default:
			s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
		return
	}

	s.writeJSONError(w, http.StatusNotFound, "Not Found")
}

func (s *ForgeStore) handlePullsCollection(w http.ResponseWriter, r *http.Request, repo *Repository) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			Head  string `json:"head"`
			Base  string `json:"base"`
		}
		if err := decodeJSONBody(r, &req); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		pr, err := s.CreatePull(repo, req.Title, req.Body, req.Head, req.Base)
		if err != nil {
			s.writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		s.writeJSON(w, http.StatusCreated, pr)
	case http.MethodGet:
		state := r.URL.Query().Get("state")
		if state == "" {
			state = "open"
		}
		page, perPage := parsePageParams(r, 30)
		repo.mu.RLock()
		out := make([]PullRequest, 0, len(repo.PullOrder))
		for i := len(repo.PullOrder) - 1; i >= 0; i-- {
			pr := repo.Pulls[repo.PullOrder[i]]
			if state == "all" || pr.State == state {
				out = append(out, *pr)
			}
		}
		repo.mu.RUnlock()
		start := (page - 1) * perPage
		if start >= len(out) {
			s.writeJSON(w, http.StatusOK, []PullRequest{})
			return
		}
		end := start + perPage
		if end > len(out) {
			end = len(out)
		}
		s.writeJSON(w, http.StatusOK, out[start:end])
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

func (s *ForgeStore) handlePullItemRoutes(w http.ResponseWriter, r *http.Request, repo *Repository, tail string) {
	parts := strings.SplitN(tail, "/", 2)
	num, err := strconv.Atoi(parts[0])
	if err != nil || num <= 0 {
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
		return
	}

	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		repo.mu.RLock()
		pr, ok := repo.Pulls[num]
		var cp PullRequest
		if ok {
			cp = *pr
		}
		repo.mu.RUnlock()
		if !ok {
			s.writeJSONError(w, http.StatusNotFound, "Not Found")
			return
		}
		s.writeJSON(w, http.StatusOK, cp)
		return
	}

	switch parts[1] {
	case "files":
		if r.Method != http.MethodGet {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		repo.mu.RLock()
		pr, ok := repo.Pulls[num]
		if !ok {
			repo.mu.RUnlock()
			s.writeJSONError(w, http.StatusNotFound, "Not Found")
			return
		}
		headSHA := pr.Head.SHA
		if !pr.Merged {
			if curHead, okH := repo.Refs["refs/heads/"+pr.Head.Ref]; okH {
				headSHA = curHead
			}
		}
		baseSHA := pr.BaseSHAAtOpen
		if !pr.Merged {
			if curBase, okB := repo.Refs["refs/heads/"+pr.Base.Ref]; okB {
				if mb := repo.findMergeBaseLocked(curBase, headSHA); mb != "" {
					baseSHA = mb
				}
			}
		}
		files := repo.diffCommitsLocked(baseSHA, headSHA)
		repo.mu.RUnlock()
		s.writeJSON(w, http.StatusOK, files)
	case "merge":
		if r.Method != http.MethodPut {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		var req struct {
			CommitTitle string `json:"commit_title"`
			MergeMethod string `json:"merge_method"`
		}
		if err := decodeJSONBody(r, &req); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		res, status, err := s.MergePull(repo, num, req.CommitTitle)
		if err != nil {
			s.writeJSONError(w, status, err.Error())
			return
		}
		s.writeJSON(w, status, res)
	default:
		s.writeJSONError(w, http.StatusNotFound, "Not Found")
	}
}
