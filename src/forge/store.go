package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	viewerID    = 1000
	viewerLogin = "adaiversen"
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.com"
	zeroOID     = "0000000000000000000000000000000000000000"
)

type Label struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type Comment struct {
	ID        int    `json:"id"`
	NodeID    string `json:"node_id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	User      User   `json:"user"`
}

type User struct {
	ID    int    `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name,omitempty"`
}

type Issue struct {
	ID        int        `json:"id"`
	NodeID    string     `json:"node_id"`
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	Labels    []Label    `json:"labels"`
	User      User       `json:"user"`
	CreatedAt string     `json:"created_at"`
	UpdatedAt string     `json:"updated_at"`
	Comments  []*Comment `json:"-"`

	// Pre-serialized JSON payload for the hot GET /issues/{n} read path.
	cachedJSON []byte
}

type PullRef struct {
	Ref   string `json:"ref"`
	Label string `json:"label"`
	SHA   string `json:"sha"`
}

type PullRequest struct {
	ID             int     `json:"id"`
	NodeID         string  `json:"node_id"`
	Number         int     `json:"number"`
	State          string  `json:"state"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	Head           PullRef `json:"head"`
	Base           PullRef `json:"base"`
	Merged         bool    `json:"merged"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSHA *string `json:"merge_commit_sha"`
	Mergeable      *bool   `json:"mergeable"`
	User           User    `json:"user"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`

	// Snapshot head/base SHAs captured at merge so /files stays stable even after merge.
	BaseSHAAtOpen string `json:"-"`
	HeadSHAAtOpen string `json:"-"`
}

type Repository struct {
	mu            sync.RWMutex
	ID            int
	NodeID        string
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string

	Objects map[string]GitObject
	Refs    map[string]string // full ref name -> 40-hex oid

	Labels      map[string]Label
	NextLabelID int
	NextNumber  int
	Issues      map[int]*Issue
	IssueOrder  []int
	Pulls       map[int]*PullRequest
	PullOrder   []int
	CommitSeq   int64
	RefGen      uint64

	// Hot-path caches invalidated on ref/object updates.
	shallowPackCache map[string][]byte
	lsRefsCache      map[string][]byte
	recvAdvCache     []byte
}

func (r *Repository) invalidateGitCachesLocked() {
	r.RefGen++
	clear(r.shallowPackCache)
	clear(r.lsRefsCache)
	r.recvAdvCache = nil
}

type RepoJSON struct {
	ID            int    `json:"id"`
	NodeID        string `json:"node_id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	Owner         User   `json:"owner"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

func (r *Repository) toJSONLocked() RepoJSON {
	return RepoJSON{
		ID:            r.ID,
		NodeID:        r.NodeID,
		Name:          r.Name,
		FullName:      r.Owner + "/" + r.Name,
		Description:   r.Description,
		Private:       r.Private,
		DefaultBranch: r.DefaultBranch,
		Owner:         User{ID: viewerID, Login: r.Owner, Name: viewerName},
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

func encodeNodeID(kind string, parts ...int) string {
	var b strings.Builder
	b.WriteString(kind)
	for _, p := range parts {
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(p))
	}
	return base64.StdEncoding.EncodeToString([]byte(b.String()))
}

func decodeNodeID(id string) (string, []int, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", nil, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return "", nil, false
	}
	nums := make([]int, 0, len(parts)-1)
	for _, s := range parts[1:] {
		v, err := strconv.Atoi(s)
		if err != nil {
			return "", nil, false
		}
		nums = append(nums, v)
	}
	return parts[0], nums, true
}

type ForgeStore struct {
	mu          sync.RWMutex
	token       string
	trialNowISO string
	trialEpoch  int64

	reposByKey  map[string]*Repository // owner/name -> repo
	reposByID   map[int]*Repository
	repoOrder   []string // insertion order of owner/name
	nextRepoID  int
	nextIssueID int
	nextPullID  int
	nextCommID  int
	reqCounter  atomic.Uint64
}

func NewForgeStore() *ForgeStore {
	nowISO := strings.TrimSpace(os.Getenv("MAKEFUN_TRIAL_NOW"))
	if nowISO == "" {
		nowISO = "2026-01-01T00:00:00Z"
	}
	return &ForgeStore{
		token:       os.Getenv("MAKEFUN_SHOP_TOKEN"),
		trialNowISO: nowISO,
		trialEpoch:  1735689600,
		reposByKey:  make(map[string]*Repository, 64),
		reposByID:   make(map[int]*Repository, 64),
		repoOrder:   make([]string, 0, 64),
		nextRepoID:  1,
		nextIssueID: 1,
		nextPullID:  1,
		nextCommID:  1,
	}
}

func (s *ForgeStore) NextRequestID() string {
	n := s.reqCounter.Add(1)
	return "req-" + strconv.FormatUint(n, 16)
}

func (s *ForgeStore) CheckAuth(authHeader string) bool {
	if s.token == "" {
		return true
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return false
	}
	return authHeader[len("Bearer "):] == s.token
}

func repoKey(owner, name string) string {
	return owner + "/" + name
}

func (s *ForgeStore) CreateRepo(owner, name, description string, isPrivate bool) (*Repository, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := repoKey(owner, name)
	if _, exists := s.reposByKey[key]; exists {
		return nil, false
	}
	id := s.nextRepoID
	s.nextRepoID++
	repo := &Repository{
		ID:               id,
		NodeID:           encodeNodeID("Repository", id),
		Owner:            owner,
		Name:             name,
		Description:      description,
		Private:          isPrivate,
		DefaultBranch:    "main",
		CreatedAt:        s.trialNowISO,
		UpdatedAt:        s.trialNowISO,
		Objects:          make(map[string]GitObject, 32),
		Refs:             make(map[string]string, 8),
		Labels:           make(map[string]Label, 8),
		NextLabelID:      1,
		NextNumber:       1,
		Issues:           make(map[int]*Issue, 16),
		IssueOrder:       make([]int, 0, 16),
		Pulls:            make(map[int]*PullRequest, 8),
		PullOrder:        make([]int, 0, 8),
		shallowPackCache: make(map[string][]byte, 8),
		lsRefsCache:      make(map[string][]byte, 8),
	}
	s.reposByKey[key] = repo
	s.reposByID[id] = repo
	s.repoOrder = append(s.repoOrder, key)
	return repo, true
}

func (s *ForgeStore) GetRepo(owner, name string) *Repository {
	s.mu.RLock()
	repo := s.reposByKey[repoKey(owner, name)]
	s.mu.RUnlock()
	return repo
}

func (s *ForgeStore) GetRepoByID(id int) *Repository {
	s.mu.RLock()
	repo := s.reposByID[id]
	s.mu.RUnlock()
	return repo
}

func (s *ForgeStore) DeleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	repo, ok := s.reposByKey[key]
	if !ok {
		return false
	}
	delete(s.reposByKey, key)
	delete(s.reposByID, repo.ID)
	for i, k := range s.repoOrder {
		if k == key {
			s.repoOrder = append(s.repoOrder[:i], s.repoOrder[i+1:]...)
			break
		}
	}
	return true
}

func (s *ForgeStore) ListViewerRepos() []RepoJSON {
	s.mu.RLock()
	repos := make([]*Repository, 0, len(s.repoOrder))
	for _, k := range s.repoOrder {
		if r := s.reposByKey[k]; r != nil && r.Owner == viewerLogin {
			repos = append(repos, r)
		}
	}
	s.mu.RUnlock()

	out := make([]RepoJSON, 0, len(repos))
	for _, r := range repos {
		r.mu.RLock()
		out = append(out, r.toJSONLocked())
		r.mu.RUnlock()
	}
	return out
}

func (s *ForgeStore) NextIssueGlobalID() int {
	s.mu.Lock()
	id := s.nextIssueID
	s.nextIssueID++
	s.mu.Unlock()
	return id
}

func (s *ForgeStore) NextPullGlobalID() int {
	s.mu.Lock()
	id := s.nextPullID
	s.nextPullID++
	s.mu.Unlock()
	return id
}

func (s *ForgeStore) NextCommentGlobalID() int {
	s.mu.Lock()
	id := s.nextCommID
	s.nextCommID++
	s.mu.Unlock()
	return id
}

func (i *Issue) rebuildJSONLocked() {
	b, _ := json.Marshal(i)
	i.cachedJSON = b
}

func (s *ForgeStore) CreateIssue(repo *Repository, title, body string, labelNames []string) (*Issue, error) {
	globalID := s.NextIssueGlobalID()
	repo.mu.Lock()
	defer repo.mu.Unlock()

	labels := make([]Label, 0, len(labelNames))
	for _, ln := range labelNames {
		lbl, ok := repo.Labels[ln]
		if !ok {
			return nil, fmt.Errorf("unknown label %q", ln)
		}
		labels = append(labels, lbl)
	}
	num := repo.NextNumber
	repo.NextNumber++
	issue := &Issue{
		ID:        globalID,
		NodeID:    encodeNodeID("Issue", repo.ID, num),
		Number:    num,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    labels,
		User:      User{ID: viewerID, Login: viewerLogin, Name: viewerName},
		CreatedAt: s.trialNowISO,
		UpdatedAt: s.trialNowISO,
		Comments:  make([]*Comment, 0),
	}
	issue.rebuildJSONLocked()
	repo.Issues[num] = issue
	repo.IssueOrder = append(repo.IssueOrder, num)
	return issue, nil
}

func (s *ForgeStore) AddComment(repo *Repository, issueNumber int, body string) (*Comment, bool) {
	cid := s.NextCommentGlobalID()
	repo.mu.Lock()
	defer repo.mu.Unlock()
	issue, ok := repo.Issues[issueNumber]
	if !ok {
		return nil, false
	}
	c := &Comment{
		ID:        cid,
		NodeID:    encodeNodeID("IssueComment", cid),
		Body:      body,
		CreatedAt: s.trialNowISO,
		UpdatedAt: s.trialNowISO,
		User:      User{ID: viewerID, Login: viewerLogin, Name: viewerName},
	}
	issue.Comments = append(issue.Comments, c)
	return c, true
}

func (s *ForgeStore) CreatePull(repo *Repository, title, body, head, base string) (*PullRequest, error) {
	globalID := s.NextPullGlobalID()
	repo.mu.Lock()
	defer repo.mu.Unlock()

	headRefName := strings.TrimPrefix(head, "refs/heads/")
	baseRefName := strings.TrimPrefix(base, "refs/heads/")
	headSHA, okH := repo.Refs["refs/heads/"+headRefName]
	if !okH {
		return nil, fmt.Errorf("head branch %q not found", headRefName)
	}
	baseSHA, okB := repo.Refs["refs/heads/"+baseRefName]
	if !okB {
		return nil, fmt.Errorf("base branch %q not found", baseRefName)
	}

	num := repo.NextNumber
	repo.NextNumber++
	mergeable := true
	pr := &PullRequest{
		ID:     globalID,
		NodeID: encodeNodeID("PullRequest", repo.ID, num),
		Number: num,
		State:  "open",
		Title:  title,
		Body:   body,
		Head: PullRef{
			Ref:   headRefName,
			Label: repo.Owner + ":" + headRefName,
			SHA:   headSHA,
		},
		Base: PullRef{
			Ref:   baseRefName,
			Label: repo.Owner + ":" + baseRefName,
			SHA:   baseSHA,
		},
		Merged:        false,
		Mergeable:     &mergeable,
		User:          User{ID: viewerID, Login: viewerLogin, Name: viewerName},
		CreatedAt:     s.trialNowISO,
		UpdatedAt:     s.trialNowISO,
		BaseSHAAtOpen: baseSHA,
		HeadSHAAtOpen: headSHA,
	}
	repo.Pulls[num] = pr
	repo.PullOrder = append(repo.PullOrder, num)
	return pr, nil
}

type MergeResult struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

func (s *ForgeStore) MergePull(repo *Repository, number int, commitTitle string) (*MergeResult, int, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()

	pr, ok := repo.Pulls[number]
	if !ok {
		return nil, 404, fmt.Errorf("Pull Request not found")
	}
	if pr.Merged || pr.State != "open" {
		return nil, 405, fmt.Errorf("Pull Request is not mergeable")
	}

	headSHA, okH := repo.Refs["refs/heads/"+pr.Head.Ref]
	if !okH {
		headSHA = pr.Head.SHA
	}
	baseSHA, okB := repo.Refs["refs/heads/"+pr.Base.Ref]
	if !okB {
		return nil, 405, fmt.Errorf("Base branch not found")
	}

	// Find merge base between baseSHA and headSHA.
	ancestorSHA := repo.findMergeBaseLocked(baseSHA, headSHA)
	if ancestorSHA == "" {
		ancestorSHA = pr.BaseSHAAtOpen
	}

	mergedFiles, conflict := repo.threeWayMergeLocked(ancestorSHA, baseSHA, headSHA)
	if conflict {
		f := false
		pr.Mergeable = &f
		return nil, 405, fmt.Errorf("Pull Request is not mergeable due to merge conflict")
	}

	treeOID := repo.writeFilesTreeLocked(mergedFiles)
	msg := commitTitle
	if msg == "" {
		msg = fmt.Sprintf("%s (#%d)", pr.Title, pr.Number)
	}
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	repo.CommitSeq++
	ts := s.trialEpoch + repo.CommitSeq
	commitBytes := buildCommitBytes(treeOID, []string{baseSHA}, viewerName+" <"+viewerEmail+">", ts, msg)
	commitOID := repo.putObjectLocked(GitObject{Type: "commit", Data: commitBytes})

	repo.Refs["refs/heads/"+pr.Base.Ref] = commitOID
	repo.invalidateGitCachesLocked()

	nowStr := s.trialNowISO
	shaCopy := commitOID
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = &nowStr
	pr.MergeCommitSHA = &shaCopy
	pr.Base.SHA = commitOID
	pr.Head.SHA = headSHA

	return &MergeResult{
		SHA:     commitOID,
		Merged:  true,
		Message: "Pull Request successfully merged",
	}, 200, nil
}

func (r *Repository) sortedRefNamesLocked() []string {
	names := make([]string, 0, len(r.Refs))
	for name := range r.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
