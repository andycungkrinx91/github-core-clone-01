package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v any, fallback int) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return fallback
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

type gqlExec struct {
	store  *ForgeStore
	doc    *gqlDocument
	vars   map[string]any
	errors []map[string]any
}

func (e *gqlExec) addErr(msg string) {
	e.errors = append(e.errors, map[string]any{"message": msg})
}

func (e *gqlExec) expandSelections(typeName string, sels []gqlSelection) []gqlSelection {
	out := make([]gqlSelection, 0, len(sels))
	for _, s := range sels {
		if s.isInlineFrag {
			if s.typeCondition == "" || s.typeCondition == typeName || s.typeCondition == "Node" {
				out = append(out, e.expandSelections(typeName, s.selection)...)
			}
			continue
		}
		if s.isFragSpread {
			if fd, ok := e.doc.fragments[s.name]; ok {
				if fd.typeCondition == "" || fd.typeCondition == typeName || fd.typeCondition == "Node" {
					out = append(out, e.expandSelections(typeName, fd.selection)...)
				}
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

func (e *gqlExec) execQuery(sels []gqlSelection) map[string]any {
	res := make(map[string]any)
	for _, s := range e.expandSelections("Query", sels) {
		args := evalArgs(s.args, e.vars)
		switch s.name {
		case "__typename":
			res[s.alias] = "Query"
		case "viewer":
			res[s.alias] = e.resolveUser(s.selection)
		case "repository":
			owner := asString(args["owner"])
			name := asString(args["name"])
			if owner != viewerLogin {
				res[s.alias] = nil
				continue
			}
			repo := e.store.GetRepo(owner, name)
			if repo == nil {
				res[s.alias] = nil
				continue
			}
			res[s.alias] = e.resolveRepository(repo, s.selection)
		case "node":
			id := asString(args["id"])
			res[s.alias] = e.resolveNode(id, s.selection)
		case "__type":
			typeName := asString(args["name"])
			res[s.alias] = e.resolveIntrospectionType(typeName, s.selection)
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'Query'", s.name))
		}
	}
	return res
}

func (e *gqlExec) execMutation(sels []gqlSelection) map[string]any {
	res := make(map[string]any)
	for _, s := range e.expandSelections("Mutation", sels) {
		args := evalArgs(s.args, e.vars)
		input := asMap(args["input"])
		switch s.name {
		case "__typename":
			res[s.alias] = "Mutation"
		case "createIssue":
			res[s.alias] = e.mutCreateIssue(input, s.selection)
		case "closeIssue":
			res[s.alias] = e.mutCloseIssue(input, s.selection)
		case "createPullRequest":
			res[s.alias] = e.mutCreatePullRequest(input, s.selection)
		case "mergePullRequest":
			res[s.alias] = e.mutMergePullRequest(input, s.selection)
		case "addComment":
			res[s.alias] = e.mutAddComment(input, s.selection)
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'Mutation'", s.name))
		}
	}
	return res
}

func (e *gqlExec) resolveUser(sels []gqlSelection) map[string]any {
	out := make(map[string]any)
	for _, s := range e.expandSelections("User", sels) {
		switch s.name {
		case "__typename":
			out[s.alias] = "User"
		case "id":
			out[s.alias] = encodeNodeID("User", viewerID)
		case "login":
			out[s.alias] = viewerLogin
		case "name":
			out[s.alias] = viewerName
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'User'", s.name))
		}
	}
	return out
}

func (e *gqlExec) resolveIntrospectionType(typeName string, sels []gqlSelection) any {
	st, ok := gqlSchemaTypes[typeName]
	if !ok {
		return nil
	}
	out := make(map[string]any)
	for _, s := range e.expandSelections("__Type", sels) {
		switch s.name {
		case "__typename":
			out[s.alias] = "__Type"
		case "name":
			out[s.alias] = st.Name
		case "kind":
			out[s.alias] = st.Kind
		case "fields":
			if st.Kind != "OBJECT" {
				out[s.alias] = nil
				continue
			}
			list := make([]map[string]any, 0, len(st.Fields))
			for _, fn := range st.Fields {
				item := make(map[string]any)
				for _, fs := range e.expandSelections("__Field", s.selection) {
					switch fs.name {
					case "name":
						item[fs.alias] = fn
					case "isDeprecated":
						item[fs.alias] = false
					case "description":
						item[fs.alias] = ""
					case "__typename":
						item[fs.alias] = "__Field"
					default:
						e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type '__Field'", fs.name))
					}
				}
				list = append(list, item)
			}
			out[s.alias] = list
		case "enumValues":
			if st.Kind != "ENUM" {
				out[s.alias] = nil
				continue
			}
			list := make([]map[string]any, 0, len(st.EnumValues))
			for _, ev := range st.EnumValues {
				item := make(map[string]any)
				for _, es := range e.expandSelections("__EnumValue", s.selection) {
					switch es.name {
					case "name":
						item[es.alias] = ev
					case "isDeprecated":
						item[es.alias] = false
					case "description":
						item[es.alias] = ""
					case "__typename":
						item[es.alias] = "__EnumValue"
					default:
						e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type '__EnumValue'", es.name))
					}
				}
				list = append(list, item)
			}
			out[s.alias] = list
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type '__Type'", s.name))
		}
	}
	return out
}

func (e *gqlExec) resolveNode(id string, sels []gqlSelection) any {
	kind, nums, ok := decodeNodeID(id)
	if !ok {
		return nil
	}
	switch kind {
	case "Repository":
		if len(nums) != 1 {
			return nil
		}
		repo := e.store.GetRepoByID(nums[0])
		if repo == nil {
			return nil
		}
		return e.resolveRepository(repo, sels)
	case "Issue":
		if len(nums) != 2 {
			return nil
		}
		repo := e.store.GetRepoByID(nums[0])
		if repo == nil {
			return nil
		}
		repo.mu.RLock()
		iss := repo.Issues[nums[1]]
		repo.mu.RUnlock()
		if iss == nil {
			return nil
		}
		return e.resolveIssue(iss, sels)
	case "PullRequest":
		if len(nums) != 2 {
			return nil
		}
		repo := e.store.GetRepoByID(nums[0])
		if repo == nil {
			return nil
		}
		repo.mu.RLock()
		pr := repo.Pulls[nums[1]]
		repo.mu.RUnlock()
		if pr == nil {
			return nil
		}
		return e.resolvePullRequest(repo, pr, sels)
	default:
		return nil
	}
}

func (e *gqlExec) resolveRepository(repo *Repository, sels []gqlSelection) map[string]any {
	out := make(map[string]any)
	for _, s := range e.expandSelections("Repository", sels) {
		args := evalArgs(s.args, e.vars)
		switch s.name {
		case "__typename":
			out[s.alias] = "Repository"
		case "id":
			out[s.alias] = repo.NodeID
		case "databaseId":
			out[s.alias] = repo.ID
		case "name":
			out[s.alias] = repo.Name
		case "nameWithOwner":
			out[s.alias] = repo.Owner + "/" + repo.Name
		case "description":
			repo.mu.RLock()
			out[s.alias] = repo.Description
			repo.mu.RUnlock()
		case "isPrivate":
			repo.mu.RLock()
			out[s.alias] = repo.Private
			repo.mu.RUnlock()
		case "owner":
			out[s.alias] = e.resolveUser(s.selection)
		case "defaultBranchRef":
			repo.mu.RLock()
			_, exists := repo.Refs["refs/heads/"+repo.DefaultBranch]
			defName := repo.DefaultBranch
			repo.mu.RUnlock()
			if !exists {
				out[s.alias] = nil
			} else {
				refObj := make(map[string]any)
				for _, rs := range e.expandSelections("Ref", s.selection) {
					switch rs.name {
					case "__typename":
						refObj[rs.alias] = "Ref"
					case "name":
						refObj[rs.alias] = defName
					default:
						e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'Ref'", rs.name))
					}
				}
				out[s.alias] = refObj
			}
		case "viewerPermission":
			out[s.alias] = "ADMIN"
		case "hasIssuesEnabled":
			out[s.alias] = true
		case "issue":
			num := asInt(args["number"], 0)
			repo.mu.RLock()
			iss := repo.Issues[num]
			repo.mu.RUnlock()
			if iss == nil {
				out[s.alias] = nil
			} else {
				out[s.alias] = e.resolveIssue(iss, s.selection)
			}
		case "issues":
			out[s.alias] = e.resolveIssueConnection(repo, args, s.selection)
		case "pullRequest":
			num := asInt(args["number"], 0)
			repo.mu.RLock()
			pr := repo.Pulls[num]
			repo.mu.RUnlock()
			if pr == nil {
				out[s.alias] = nil
			} else {
				out[s.alias] = e.resolvePullRequest(repo, pr, s.selection)
			}
		case "pullRequests":
			out[s.alias] = e.resolvePullRequestConnection(repo, args, s.selection)
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'Repository'", s.name))
		}
	}
	return out
}

func encodeCursor(idx int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(idx)))
}

func decodeCursor(cur string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(cur)
	if err != nil {
		return 0, false
	}
	if !strings.HasPrefix(string(raw), "cursor:") {
		return 0, false
	}
	idx, err := strconv.Atoi(strings.TrimPrefix(string(raw), "cursor:"))
	if err != nil || idx < 0 {
		return 0, false
	}
	return idx, true
}

func (e *gqlExec) resolveIssueConnection(repo *Repository, args map[string]any, sels []gqlSelection) map[string]any {
	first := asInt(args["first"], 100)
	if first < 0 {
		first = 0
	}
	afterStr := asString(args["after"])
	stateFilter := map[string]bool{}
	if stRaw, ok := args["states"]; ok {
		if list, ok := stRaw.([]any); ok {
			for _, v := range list {
				if sv := strings.ToLower(asString(v)); sv != "" {
					stateFilter[sv] = true
				}
			}
		} else if sv := strings.ToLower(asString(stRaw)); sv != "" {
			stateFilter[sv] = true
		}
	}
	desc := false
	if ob := asMap(args["orderBy"]); ob != nil {
		if strings.EqualFold(asString(ob["direction"]), "DESC") {
			desc = true
		}
	}

	repo.mu.RLock()
	filtered := make([]*Issue, 0, len(repo.IssueOrder))
	if desc {
		for i := len(repo.IssueOrder) - 1; i >= 0; i-- {
			iss := repo.Issues[repo.IssueOrder[i]]
			if len(stateFilter) == 0 || stateFilter[iss.State] {
				filtered = append(filtered, iss)
			}
		}
	} else {
		for _, num := range repo.IssueOrder {
			iss := repo.Issues[num]
			if len(stateFilter) == 0 || stateFilter[iss.State] {
				filtered = append(filtered, iss)
			}
		}
	}
	repo.mu.RUnlock()

	start := 0
	if afterStr != "" {
		if idx, ok := decodeCursor(afterStr); ok {
			start = idx + 1
		}
	}
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + first
	if end > len(filtered) {
		end = len(filtered)
	}
	page := filtered[start:end]
	hasNext := end < len(filtered)
	var endCursor any = nil
	if len(page) > 0 {
		endCursor = encodeCursor(end - 1)
	}

	out := make(map[string]any)
	for _, s := range e.expandSelections("IssueConnection", sels) {
		switch s.name {
		case "__typename":
			out[s.alias] = "IssueConnection"
		case "nodes":
			nodes := make([]map[string]any, 0, len(page))
			for _, iss := range page {
				nodes = append(nodes, e.resolveIssue(iss, s.selection))
			}
			out[s.alias] = nodes
		case "pageInfo":
			pi := make(map[string]any)
			for _, ps := range e.expandSelections("PageInfo", s.selection) {
				switch ps.name {
				case "__typename":
					pi[ps.alias] = "PageInfo"
				case "hasNextPage":
					pi[ps.alias] = hasNext
				case "endCursor":
					pi[ps.alias] = endCursor
				default:
					e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'PageInfo'", ps.name))
				}
			}
			out[s.alias] = pi
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'IssueConnection'", s.name))
		}
	}
	return out
}

func (e *gqlExec) resolveIssue(iss *Issue, sels []gqlSelection) map[string]any {
	out := make(map[string]any)
	for _, s := range e.expandSelections("Issue", sels) {
		switch s.name {
		case "__typename":
			out[s.alias] = "Issue"
		case "id":
			out[s.alias] = iss.NodeID
		case "number":
			out[s.alias] = iss.Number
		case "title":
			out[s.alias] = iss.Title
		case "body":
			out[s.alias] = iss.Body
		case "state":
			out[s.alias] = strings.ToUpper(iss.State)
		case "createdAt":
			out[s.alias] = iss.CreatedAt
		case "updatedAt":
			out[s.alias] = iss.UpdatedAt
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'Issue'", s.name))
		}
	}
	return out
}

func gqlPullState(pr *PullRequest) string {
	if pr.Merged {
		return "MERGED"
	}
	return strings.ToUpper(pr.State)
}

func (e *gqlExec) resolvePullRequestConnection(repo *Repository, args map[string]any, sels []gqlSelection) map[string]any {
	first := asInt(args["first"], 100)
	if first < 0 {
		first = 0
	}
	repo.mu.RLock()
	list := make([]*PullRequest, 0, len(repo.PullOrder))
	for i := len(repo.PullOrder) - 1; i >= 0; i-- {
		list = append(list, repo.Pulls[repo.PullOrder[i]])
	}
	repo.mu.RUnlock()

	if len(list) > first {
		list = list[:first]
	}

	out := make(map[string]any)
	for _, s := range e.expandSelections("PullRequestConnection", sels) {
		switch s.name {
		case "__typename":
			out[s.alias] = "PullRequestConnection"
		case "nodes":
			nodes := make([]map[string]any, 0, len(list))
			for _, pr := range list {
				nodes = append(nodes, e.resolvePullRequest(repo, pr, s.selection))
			}
			out[s.alias] = nodes
		case "pageInfo":
			pi := make(map[string]any)
			for _, ps := range e.expandSelections("PageInfo", s.selection) {
				switch ps.name {
				case "hasNextPage":
					pi[ps.alias] = false
				case "endCursor":
					pi[ps.alias] = nil
				}
			}
			out[s.alias] = pi
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'PullRequestConnection'", s.name))
		}
	}
	return out
}

func (e *gqlExec) resolvePullRequest(repo *Repository, pr *PullRequest, sels []gqlSelection) map[string]any {
	out := make(map[string]any)
	for _, s := range e.expandSelections("PullRequest", sels) {
		args := evalArgs(s.args, e.vars)
		switch s.name {
		case "__typename":
			out[s.alias] = "PullRequest"
		case "id":
			out[s.alias] = pr.NodeID
		case "number":
			out[s.alias] = pr.Number
		case "title":
			out[s.alias] = pr.Title
		case "body":
			out[s.alias] = pr.Body
		case "headRefName":
			out[s.alias] = pr.Head.Ref
		case "baseRefName":
			out[s.alias] = pr.Base.Ref
		case "state":
			out[s.alias] = gqlPullState(pr)
		case "merged":
			out[s.alias] = pr.Merged
		case "mergedAt":
			out[s.alias] = pr.MergedAt
		case "mergeable":
			if pr.Mergeable != nil && !*pr.Mergeable {
				out[s.alias] = "CONFLICTING"
			} else {
				out[s.alias] = "MERGEABLE"
			}
		case "files":
			first := asInt(args["first"], 100)
			repo.mu.RLock()
			headSHA := pr.Head.SHA
			if !pr.Merged {
				if h, ok := repo.Refs["refs/heads/"+pr.Head.Ref]; ok {
					headSHA = h
				}
			}
			baseSHA := pr.BaseSHAAtOpen
			if !pr.Merged {
				if b, ok := repo.Refs["refs/heads/"+pr.Base.Ref]; ok {
					if mb := repo.findMergeBaseLocked(b, headSHA); mb != "" {
						baseSHA = mb
					}
				}
			}
			diffs := repo.diffCommitsLocked(baseSHA, headSHA)
			repo.mu.RUnlock()
			if len(diffs) > first {
				diffs = diffs[:first]
			}
			conn := make(map[string]any)
			for _, fs := range e.expandSelections("PullRequestChangedFileConnection", s.selection) {
				switch fs.name {
				case "__typename":
					conn[fs.alias] = "PullRequestChangedFileConnection"
				case "nodes":
					nodes := make([]map[string]any, 0, len(diffs))
					for _, d := range diffs {
						item := make(map[string]any)
						for _, nf := range e.expandSelections("PullRequestChangedFile", fs.selection) {
							switch nf.name {
							case "__typename":
								item[nf.alias] = "PullRequestChangedFile"
							case "path":
								item[nf.alias] = d.Filename
							case "additions":
								item[nf.alias] = d.Additions
							case "deletions":
								item[nf.alias] = d.Deletions
							default:
								e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'PullRequestChangedFile'", nf.name))
							}
						}
						nodes = append(nodes, item)
					}
					conn[fs.alias] = nodes
				default:
					e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'PullRequestChangedFileConnection'", fs.name))
				}
			}
			out[s.alias] = conn
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'PullRequest'", s.name))
		}
	}
	return out
}

func (e *gqlExec) mutCreateIssue(input map[string]any, sels []gqlSelection) any {
	repoID := asString(input["repositoryId"])
	kind, nums, ok := decodeNodeID(repoID)
	if !ok || kind != "Repository" || len(nums) != 1 {
		e.addErr("Invalid repositoryId")
		return nil
	}
	repo := e.store.GetRepoByID(nums[0])
	if repo == nil {
		e.addErr("Repository not found")
		return nil
	}
	iss, err := e.store.CreateIssue(repo, asString(input["title"]), asString(input["body"]), nil)
	if err != nil {
		e.addErr(err.Error())
		return nil
	}
	out := make(map[string]any)
	for _, s := range sels {
		switch s.name {
		case "issue":
			out[s.alias] = e.resolveIssue(iss, s.selection)
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'CreateIssuePayload'", s.name))
		}
	}
	return out
}

func (e *gqlExec) mutCloseIssue(input map[string]any, sels []gqlSelection) any {
	issueID := asString(input["issueId"])
	kind, nums, ok := decodeNodeID(issueID)
	if !ok || kind != "Issue" || len(nums) != 2 {
		e.addErr("Invalid issueId")
		return nil
	}
	repo := e.store.GetRepoByID(nums[0])
	if repo == nil {
		e.addErr("Repository not found")
		return nil
	}
	repo.mu.Lock()
	iss, exists := repo.Issues[nums[1]]
	if exists {
		iss.State = "closed"
		iss.rebuildJSONLocked()
	}
	repo.mu.Unlock()
	if !exists {
		e.addErr("Issue not found")
		return nil
	}
	out := make(map[string]any)
	for _, s := range sels {
		switch s.name {
		case "issue":
			out[s.alias] = e.resolveIssue(iss, s.selection)
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'CloseIssuePayload'", s.name))
		}
	}
	return out
}

func (e *gqlExec) mutCreatePullRequest(input map[string]any, sels []gqlSelection) any {
	repoID := asString(input["repositoryId"])
	kind, nums, ok := decodeNodeID(repoID)
	if !ok || kind != "Repository" || len(nums) != 1 {
		e.addErr("Invalid repositoryId")
		return nil
	}
	repo := e.store.GetRepoByID(nums[0])
	if repo == nil {
		e.addErr("Repository not found")
		return nil
	}
	pr, err := e.store.CreatePull(repo, asString(input["title"]), asString(input["body"]), asString(input["headRefName"]), asString(input["baseRefName"]))
	if err != nil {
		e.addErr(err.Error())
		return nil
	}
	out := make(map[string]any)
	for _, s := range sels {
		switch s.name {
		case "pullRequest":
			out[s.alias] = e.resolvePullRequest(repo, pr, s.selection)
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'CreatePullRequestPayload'", s.name))
		}
	}
	return out
}

func (e *gqlExec) mutMergePullRequest(input map[string]any, sels []gqlSelection) any {
	prID := asString(input["pullRequestId"])
	kind, nums, ok := decodeNodeID(prID)
	if !ok || kind != "PullRequest" || len(nums) != 2 {
		e.addErr("Invalid pullRequestId")
		return nil
	}
	repo := e.store.GetRepoByID(nums[0])
	if repo == nil {
		e.addErr("Repository not found")
		return nil
	}
	if _, _, err := e.store.MergePull(repo, nums[1], ""); err != nil {
		e.addErr(err.Error())
		return nil
	}
	repo.mu.RLock()
	pr := repo.Pulls[nums[1]]
	repo.mu.RUnlock()
	out := make(map[string]any)
	for _, s := range sels {
		switch s.name {
		case "pullRequest":
			out[s.alias] = e.resolvePullRequest(repo, pr, s.selection)
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'MergePullRequestPayload'", s.name))
		}
	}
	return out
}

func (e *gqlExec) mutAddComment(input map[string]any, sels []gqlSelection) any {
	subjID := asString(input["subjectId"])
	body := asString(input["body"])
	kind, nums, ok := decodeNodeID(subjID)
	if !ok || kind != "Issue" || len(nums) != 2 {
		e.addErr("Invalid subjectId")
		return nil
	}
	repo := e.store.GetRepoByID(nums[0])
	if repo == nil {
		e.addErr("Repository not found")
		return nil
	}
	c, ok := e.store.AddComment(repo, nums[1], body)
	if !ok {
		e.addErr("Issue not found")
		return nil
	}
	out := make(map[string]any)
	for _, s := range sels {
		switch s.name {
		case "commentEdge":
			edge := make(map[string]any)
			for _, es := range s.selection {
				switch es.name {
				case "node":
					node := make(map[string]any)
					for _, ns := range es.selection {
						switch ns.name {
						case "id":
							node[ns.alias] = c.NodeID
						case "body":
							node[ns.alias] = c.Body
						case "__typename":
							node[ns.alias] = "IssueComment"
						default:
							e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'IssueComment'", ns.name))
						}
					}
					edge[es.alias] = node
				}
			}
			out[s.alias] = edge
		default:
			e.addErr(fmt.Sprintf("Field '%s' doesn't exist on type 'AddCommentPayload'", s.name))
		}
	}
	return out
}

type gqlRequestPayload struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

func (s *ForgeStore) executeOneGraphQL(req gqlRequestPayload) map[string]any {
	doc, err := parseGraphQLDocument(req.Query)
	if err != nil {
		return map[string]any{
			"data":   nil,
			"errors": []map[string]any{{"message": err.Error()}},
		}
	}
	if len(doc.ops) == 0 {
		return map[string]any{
			"data":   nil,
			"errors": []map[string]any{{"message": "No GraphQL operation found"}},
		}
	}
	op := doc.ops[0]
	if req.OperationName != "" {
		for _, cand := range doc.ops {
			if cand.name == req.OperationName {
				op = cand
				break
			}
		}
	}

	vars := make(map[string]any, len(req.Variables)+len(op.varDefs))
	for _, vd := range op.varDefs {
		if vd.hasDefault {
			vars[vd.name] = vd.defaultVal
		}
	}
	for k, v := range req.Variables {
		vars[k] = v
	}

	exec := &gqlExec{store: s, doc: doc, vars: vars}
	var data map[string]any
	if op.opType == "mutation" {
		data = exec.execMutation(op.selection)
	} else {
		data = exec.execQuery(op.selection)
	}

	resp := map[string]any{"data": data}
	if len(exec.errors) > 0 {
		resp["errors"] = exec.errors
	}
	return resp
}

func (s *ForgeStore) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if !s.CheckAuth(r.Header.Get("Authorization")) {
		s.writeJSONError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if r.Method != http.MethodPost {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBodyBytes))
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "Failed to read request body")
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		s.writeJSONError(w, http.StatusBadRequest, "Empty request body")
		return
	}

	if trimmed[0] == '[' {
		var batch []gqlRequestPayload
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		results := make([]map[string]any, len(batch))
		for i, op := range batch {
			results[i] = s.executeOneGraphQL(op)
		}
		s.writeJSON(w, http.StatusOK, results)
		return
	}

	var single gqlRequestPayload
	if err := json.Unmarshal(trimmed, &single); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	s.writeJSON(w, http.StatusOK, s.executeOneGraphQL(single))
}
