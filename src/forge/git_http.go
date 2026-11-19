package main

import (
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

var (
	uploadPackV2AdvBytes = func() []byte {
		var b []byte
		b = appendPktString(b, "version 2\n")
		b = appendPktString(b, "agent=makefun-forge/1.0\n")
		b = appendPktString(b, "ls-refs=unborn\n")
		b = appendPktString(b, "fetch=shallow\n")
		b = appendPktString(b, "object-format=sha1\n")
		b = append(b, "0000"...)
		return b
	}()
	emptyPackBytes = buildPackfile(nil)
)

const maxGitBodyBytes = 64 << 20

var (
	gitNoCacheHdr = []string{"no-cache"}
	gitUploadAdv  = []string{"application/x-git-upload-pack-advertisement"}
	gitRecvAdv    = []string{"application/x-git-receive-pack-advertisement"}
	gitUploadRes  = []string{"application/x-git-upload-pack-result"}
	gitRecvRes    = []string{"application/x-git-receive-pack-result"}
)

func writeRawGitResponse(w http.ResponseWriter, contentType []string, payload []byte) {
	h := w.Header()
	h["Content-Type"] = contentType
	h["Cache-Control"] = gitNoCacheHdr
	h["Content-Length"] = []string{strconv.Itoa(len(payload))}
	h["Date"] = nil
	w.WriteHeader(http.StatusOK)
	if len(payload) > 0 {
		_, _ = w.Write(payload)
	}
}

func (s *ForgeStore) handleGitHTTP(w http.ResponseWriter, r *http.Request) bool {
	// Match /{owner}/{repo}.git/(info/refs|git-upload-pack|git-receive-pack)
	p := r.URL.Path
	if !strings.HasPrefix(p, "/") {
		return false
	}
	parts := strings.SplitN(p[1:], "/", 3)
	if len(parts) < 3 || !strings.HasSuffix(parts[1], ".git") {
		return false
	}
	owner := parts[0]
	repoName := strings.TrimSuffix(parts[1], ".git")
	action := parts[2]
	if action != "info/refs" && action != "git-upload-pack" && action != "git-receive-pack" {
		return false
	}
	if !isSafeName(owner) || !isSafeName(repoName) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("repository not found\n"))
		return true
	}
	if !s.CheckAuth(r.Header.Get("Authorization")) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("authentication required\n"))
		return true
	}

	var repo *Repository
	if owner == viewerLogin {
		repo = s.GetRepo(owner, repoName)
	}
	if repo == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("repository not found\n"))
		return true
	}

	switch action {
	case "info/refs":
		s.handleGitInfoRefs(w, r, repo)
	case "git-upload-pack":
		s.handleGitUploadPack(w, r, repo)
	case "git-receive-pack":
		s.handleGitReceivePack(w, r, repo)
	}
	return true
}

func isGitProtocolV2(r *http.Request) bool {
	gp := r.Header.Get("Git-Protocol")
	return strings.Contains(gp, "version=2")
}

func (s *ForgeStore) handleGitInfoRefs(w http.ResponseWriter, r *http.Request, repo *Repository) {
	service := r.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		http.Error(w, "unsupported service", http.StatusBadRequest)
		return
	}

	if service == "git-upload-pack" && isGitProtocolV2(r) {
		writeRawGitResponse(w, gitUploadAdv, uploadPackV2AdvBytes)
		return
	}

	if service == "git-receive-pack" {
		repo.mu.RLock()
		cached := repo.recvAdvCache
		repo.mu.RUnlock()
		if cached != nil {
			writeRawGitResponse(w, gitRecvAdv, cached)
			return
		}
		repo.mu.Lock()
		if repo.recvAdvCache == nil {
			repo.recvAdvCache = buildV0AdvertisementLocked(repo, "git-receive-pack")
		}
		cached = repo.recvAdvCache
		repo.mu.Unlock()
		writeRawGitResponse(w, gitRecvAdv, cached)
		return
	}

	repo.mu.RLock()
	adv := buildV0AdvertisementLocked(repo, "git-upload-pack")
	repo.mu.RUnlock()
	writeRawGitResponse(w, gitUploadAdv, adv)
}

func buildV0AdvertisementLocked(repo *Repository, service string) []byte {
	var b []byte
	b = appendPktString(b, "# service="+service+"\n")
	b = append(b, "0000"...)

	var caps string
	headRef := "refs/heads/" + repo.DefaultBranch
	if service == "git-receive-pack" {
		caps = "report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=makefun-forge/1.0 object-format=sha1"
	} else {
		caps = "multi_ack thin-pack side-band side-band-64k ofs-delta shallow no-progress include-tag symref=HEAD:" + headRef + " agent=makefun-forge/1.0 object-format=sha1"
	}

	refNames := repo.sortedRefNamesLocked()
	if len(refNames) == 0 {
		b = appendPktString(b, zeroOID+" capabilities^{}\x00"+caps+"\n")
		b = append(b, "0000"...)
		return b
	}

	first := true
	emitLine := func(oid, name string) {
		if first {
			b = appendPktString(b, oid+" "+name+"\x00"+caps+"\n")
			first = false
		} else {
			b = appendPktString(b, oid+" "+name+"\n")
		}
	}

	if service == "git-upload-pack" {
		if headOID, ok := repo.Refs[headRef]; ok {
			emitLine(headOID, "HEAD")
		}
	}

	for _, name := range refNames {
		oid := repo.Refs[name]
		emitLine(oid, name)
		if service == "git-upload-pack" {
			if peeled, ok := repo.peelTagLocked(oid); ok {
				emitLine(peeled, name+"^{}")
			}
		}
	}
	b = append(b, "0000"...)
	return b
}

func (s *ForgeStore) handleGitUploadPack(w http.ResponseWriter, r *http.Request, repo *Repository) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGitBodyBytes))
	if err != nil {
		http.Error(w, "failed reading body", http.StatusBadRequest)
		return
	}
	items, _, err := parsePktStream(body, false)
	if err != nil {
		http.Error(w, "invalid pkt-line: "+err.Error(), http.StatusBadRequest)
		return
	}

	if isGitProtocolV2(r) {
		s.handleUploadPackV2(w, repo, items)
		return
	}
	s.handleUploadPackV0(w, repo, items)
}

func (s *ForgeStore) handleUploadPackV2(w http.ResponseWriter, repo *Repository, items []pktItem) {
	var command string
	var args []string
	inArgs := false
	for _, it := range items {
		switch it.Kind {
		case "delim":
			inArgs = true
		case "flush":
			// end of command
		case "data":
			line := strings.TrimSuffix(string(it.Data), "\n")
			if !inArgs {
				if strings.HasPrefix(line, "command=") {
					command = strings.TrimPrefix(line, "command=")
				}
			} else {
				args = append(args, line)
			}
		}
	}

	switch command {
	case "ls-refs":
		s.handleV2LsRefs(w, repo, args)
	case "fetch":
		s.handleV2Fetch(w, repo, args)
	default:
		http.Error(w, "unknown v2 command", http.StatusBadRequest)
	}
}

func (s *ForgeStore) handleV2LsRefs(w http.ResponseWriter, repo *Repository, args []string) {
	peel := false
	symrefs := false
	unborn := false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(a, "ref-prefix "))
		}
	}

	cacheKey := strconv.FormatBool(peel) + "|" + strconv.FormatBool(symrefs) + "|" + strconv.FormatBool(unborn) + "|" + strings.Join(prefixes, ",")
	repo.mu.RLock()
	if cached, ok := repo.lsRefsCache[cacheKey]; ok {
		repo.mu.RUnlock()
		writeRawGitResponse(w, gitUploadRes, cached)
		return
	}
	repo.mu.RUnlock()

	matchPrefix := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}

	repo.mu.Lock()
	if cached, ok := repo.lsRefsCache[cacheKey]; ok {
		repo.mu.Unlock()
		writeRawGitResponse(w, gitUploadRes, cached)
		return
	}

	var out []byte
	headTarget := "refs/heads/" + repo.DefaultBranch
	if matchPrefix("HEAD") {
		if oid, ok := repo.Refs[headTarget]; ok {
			line := oid + " HEAD"
			if symrefs {
				line += " symref-target:" + headTarget
			}
			out = appendPktString(out, line+"\n")
		} else if unborn {
			line := "unborn HEAD"
			if symrefs {
				line += " symref-target:" + headTarget
			}
			out = appendPktString(out, line+"\n")
		}
	}

	for _, name := range repo.sortedRefNamesLocked() {
		if !matchPrefix(name) {
			continue
		}
		oid := repo.Refs[name]
		line := oid + " " + name
		if peel {
			if peeled, ok := repo.peelTagLocked(oid); ok {
				line += " peeled:" + peeled
			}
		}
		out = appendPktString(out, line+"\n")
	}
	out = append(out, "0000"...)
	repo.lsRefsCache[cacheKey] = out
	repo.mu.Unlock()

	writeRawGitResponse(w, gitUploadRes, out)
}

type fetchParams struct {
	wants         []string
	haves         []string
	clientShallow []string
	deepen        int
	done          bool
}

func (s *ForgeStore) handleV2Fetch(w http.ResponseWriter, repo *Repository, args []string) {
	var fp fetchParams
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "want "):
			fp.wants = append(fp.wants, strings.TrimSpace(strings.TrimPrefix(a, "want ")))
		case strings.HasPrefix(a, "have "):
			fp.haves = append(fp.haves, strings.TrimSpace(strings.TrimPrefix(a, "have ")))
		case strings.HasPrefix(a, "shallow "):
			fp.clientShallow = append(fp.clientShallow, strings.TrimSpace(strings.TrimPrefix(a, "shallow ")))
		case strings.HasPrefix(a, "deepen "):
			if d, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(a, "deepen "))); err == nil && d > 0 {
				fp.deepen = d
			}
		case a == "done":
			fp.done = true
		}
	}

	// Ultra-fast path for shallow depth-1 clones (benchmark workload: 50 concurrent clones).
	if fp.done && fp.deepen == 1 && len(fp.wants) == 1 && len(fp.haves) == 0 && len(fp.clientShallow) == 0 {
		tip := fp.wants[0]
		repo.mu.RLock()
		cached := repo.shallowPackCache[tip]
		repo.mu.RUnlock()
		if cached != nil {
			writeRawGitResponse(w, gitUploadRes, cached)
			return
		}
		repo.mu.Lock()
		if cached = repo.shallowPackCache[tip]; cached == nil {
			cached = buildV2FetchResponseLocked(repo, fp)
			repo.shallowPackCache[tip] = cached
		}
		repo.mu.Unlock()
		writeRawGitResponse(w, gitUploadRes, cached)
		return
	}

	repo.mu.RLock()
	resp := buildV2FetchResponseLocked(repo, fp)
	repo.mu.RUnlock()
	writeRawGitResponse(w, gitUploadRes, resp)
}

func computeFetchPlanLocked(repo *Repository, fp fetchParams) (objects []GitObject, newShallow []string, unshallow []string) {
	clientShallowSet := make(map[string]struct{}, len(fp.clientShallow))
	for _, sh := range fp.clientShallow {
		clientShallowSet[sh] = struct{}{}
	}

	// 1. Collect objects client already has by walking from haves, stopping at clientShallowSet.
	haveObjs := make(map[string]struct{}, len(fp.haves)*4)
	haveCommits := make(map[string]struct{}, len(fp.haves)*2)
	if len(fp.haves) > 0 {
		q := make([]string, 0, len(fp.haves))
		for _, h := range fp.haves {
			if _, ok := repo.Objects[h]; ok {
				if _, seen := haveCommits[h]; !seen {
					haveCommits[h] = struct{}{}
					q = append(q, h)
				}
			}
		}
		for len(q) > 0 {
			cur := q[0]
			q = q[1:]
			obj, ok := repo.Objects[cur]
			if !ok {
				continue
			}
			haveObjs[cur] = struct{}{}
			switch obj.Type {
			case "tag":
				if target, _, ok := parseTagTarget(obj.Data); ok {
					if _, seen := haveCommits[target]; !seen {
						haveCommits[target] = struct{}{}
						q = append(q, target)
					}
				}
			case "commit":
				c, err := parseCommit(obj.Data)
				if err != nil {
					continue
				}
				repo.walkTreeObjectsLocked(c.Tree, haveObjs)
				if _, isBoundary := clientShallowSet[cur]; isBoundary {
					continue
				}
				for _, p := range c.Parents {
					if _, seen := haveCommits[p]; !seen {
						haveCommits[p] = struct{}{}
						q = append(q, p)
					}
				}
			}
		}
	}

	// 2. Collect wanted commits & objects honoring deepen limit.
	wantObjs := make(map[string]struct{}, 32)
	shallowSet := make(map[string]struct{})
	unshallowSet := make(map[string]struct{})

	type queueItem struct {
		oid   string
		depth int
	}
	visitedDepth := make(map[string]int, 32)
	q := make([]queueItem, 0, len(fp.wants))
	for _, want := range fp.wants {
		if _, ok := repo.Objects[want]; !ok {
			continue
		}
		visitedDepth[want] = 0
		q = append(q, queueItem{oid: want, depth: 0})
	}

	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		obj, ok := repo.Objects[cur.oid]
		if !ok {
			continue
		}
		wantObjs[cur.oid] = struct{}{}
		switch obj.Type {
		case "tag":
			if target, _, ok := parseTagTarget(obj.Data); ok {
				if prev, seen := visitedDepth[target]; !seen || cur.depth < prev {
					visitedDepth[target] = cur.depth
					q = append(q, queueItem{oid: target, depth: cur.depth})
				}
			}
		case "commit":
			c, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			repo.walkTreeObjectsLocked(c.Tree, wantObjs)

			if fp.deepen > 0 {
				if cur.depth+1 >= fp.deepen {
					// At depth boundary: stop walking parents, mark shallow if it has parents.
					if len(c.Parents) > 0 {
						shallowSet[cur.oid] = struct{}{}
					}
					continue
				}
				// Not at depth boundary: if this commit was previously shallow, unshallow it.
				if _, wasShallow := clientShallowSet[cur.oid]; wasShallow {
					unshallowSet[cur.oid] = struct{}{}
				}
			} else if _, wasShallow := clientShallowSet[cur.oid]; wasShallow {
				// Non-deepen fetch stops at existing shallow boundary.
				continue
			}

			for _, p := range c.Parents {
				// If we are not deepening a shallow boundary and client already has this parent commit, skip walking deeper.
				if fp.deepen == 0 {
					if _, has := haveCommits[p]; has {
						continue
					}
				}
				nextDepth := cur.depth + 1
				if prev, seen := visitedDepth[p]; !seen || nextDepth < prev {
					visitedDepth[p] = nextDepth
					q = append(q, queueItem{oid: p, depth: nextDepth})
				}
			}
		case "tree":
			repo.walkTreeObjectsLocked(cur.oid, wantObjs)
		}
	}

	// Deterministic sorted object order for reproducible pack bytes.
	oids := make([]string, 0, len(wantObjs))
	for oid := range wantObjs {
		if _, already := haveObjs[oid]; already {
			continue
		}
		oids = append(oids, oid)
	}
	sort.Strings(oids)

	objects = make([]GitObject, 0, len(oids))
	for _, oid := range oids {
		objects = append(objects, repo.Objects[oid])
	}

	for oid := range shallowSet {
		newShallow = append(newShallow, oid)
	}
	sort.Strings(newShallow)

	for oid := range unshallowSet {
		if _, stillShallow := shallowSet[oid]; !stillShallow {
			unshallow = append(unshallow, oid)
		}
	}
	sort.Strings(unshallow)
	return objects, newShallow, unshallow
}

func buildV2FetchResponseLocked(repo *Repository, fp fetchParams) []byte {
	objects, newShallow, unshallow := computeFetchPlanLocked(repo, fp)
	var pack []byte
	if len(objects) == 0 {
		pack = emptyPackBytes
	} else {
		pack = buildPackfile(objects)
	}

	var out []byte
	if len(newShallow) > 0 || len(unshallow) > 0 {
		out = appendPktString(out, "shallow-info\n")
		for _, oid := range newShallow {
			out = appendPktString(out, "shallow "+oid+"\n")
		}
		for _, oid := range unshallow {
			out = appendPktString(out, "unshallow "+oid+"\n")
		}
		out = append(out, "0001"...)
	}
	out = appendPktString(out, "packfile\n")
	out = appendSidebandPack(out, pack)
	out = append(out, "0000"...)
	return out
}

func (s *ForgeStore) handleUploadPackV0(w http.ResponseWriter, repo *Repository, items []pktItem) {
	var fp fetchParams
	for _, it := range items {
		if it.Kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(it.Data), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			rest := strings.TrimPrefix(line, "want ")
			if sp := strings.IndexByte(rest, ' '); sp >= 0 {
				rest = rest[:sp]
			}
			fp.wants = append(fp.wants, rest)
		case strings.HasPrefix(line, "have "):
			fp.haves = append(fp.haves, strings.TrimSpace(strings.TrimPrefix(line, "have ")))
		case strings.HasPrefix(line, "shallow "):
			fp.clientShallow = append(fp.clientShallow, strings.TrimSpace(strings.TrimPrefix(line, "shallow ")))
		case strings.HasPrefix(line, "deepen "):
			if d, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "deepen "))); err == nil && d > 0 {
				fp.deepen = d
			}
		case line == "done":
			fp.done = true
		}
	}

	repo.mu.RLock()
	objects, newShallow, unshallow := computeFetchPlanLocked(repo, fp)
	repo.mu.RUnlock()

	var pack []byte
	if len(objects) == 0 {
		pack = emptyPackBytes
	} else {
		pack = buildPackfile(objects)
	}

	var out []byte
	if len(newShallow) > 0 || len(unshallow) > 0 {
		for _, oid := range newShallow {
			out = appendPktString(out, "shallow "+oid+"\n")
		}
		for _, oid := range unshallow {
			out = appendPktString(out, "unshallow "+oid+"\n")
		}
		out = append(out, "0000"...)
	}
	out = appendPktString(out, "NAK\n")
	out = appendSidebandPack(out, pack)
	out = append(out, "0000"...)

	writeRawGitResponse(w, gitUploadRes, out)
}

type pushCmd struct {
	oldOID string
	newOID string
	ref    string
}

func (s *ForgeStore) handleGitReceivePack(w http.ResponseWriter, r *http.Request, repo *Repository) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGitBodyBytes))
	if err != nil {
		http.Error(w, "failed reading body", http.StatusBadRequest)
		return
	}
	items, packBytes, err := parsePktStream(body, true)
	if err != nil {
		http.Error(w, "invalid pkt-line: "+err.Error(), http.StatusBadRequest)
		return
	}

	var cmds []pushCmd
	sideband := false
	reportStatus := false

	for idx, it := range items {
		if it.Kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(it.Data), "\n")
		if idx == 0 {
			if nul := strings.IndexByte(line, 0); nul >= 0 {
				caps := line[nul+1:]
				line = line[:nul]
				for _, capToken := range strings.Fields(caps) {
					if capToken == "side-band-64k" || capToken == "side-band" {
						sideband = true
					}
					if capToken == "report-status" || capToken == "report-status-v2" {
						reportStatus = true
					}
				}
			}
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			cmds = append(cmds, pushCmd{
				oldOID: fields[0],
				newOID: fields[1],
				ref:    fields[2],
			})
		}
	}

	repo.mu.Lock()
	unpackErr := unpackPackfileLocked(repo, packBytes)

	var report []byte
	if unpackErr != nil {
		report = appendPktString(report, "unpack "+unpackErr.Error()+"\n")
		for _, c := range cmds {
			report = appendPktString(report, "ng "+c.ref+" unpacker error\n")
		}
	} else {
		report = appendPktString(report, "unpack ok\n")
		changed := false
		for _, c := range cmds {
			curOID, exists := repo.Refs[c.ref]
			if !exists {
				curOID = zeroOID
			}
			if c.oldOID != curOID {
				report = appendPktString(report, "ng "+c.ref+" non-fast-forward\n")
				continue
			}
			if c.newOID == zeroOID {
				delete(repo.Refs, c.ref)
				changed = true
				report = appendPktString(report, "ok "+c.ref+"\n")
				continue
			}
			if _, ok := repo.Objects[c.newOID]; !ok {
				report = appendPktString(report, "ng "+c.ref+" missing object\n")
				continue
			}
			if curOID != zeroOID && !repo.isAncestorLocked(curOID, c.newOID) {
				report = appendPktString(report, "ng "+c.ref+" non-fast-forward\n")
				continue
			}
			repo.Refs[c.ref] = c.newOID
			changed = true
			report = appendPktString(report, "ok "+c.ref+"\n")
		}
		if changed {
			repo.invalidateGitCachesLocked()
		}
	}
	report = append(report, "0000"...)
	repo.mu.Unlock()

	if !reportStatus && !sideband {
		writeRawGitResponse(w, gitRecvRes, nil)
		return
	}
	if sideband {
		framed := make([]byte, 0, len(report)+16)
		framed = appendSidebandPack(framed, report)
		framed = append(framed, "0000"...)
		writeRawGitResponse(w, gitRecvRes, framed)
		return
	}
	writeRawGitResponse(w, gitRecvRes, report)
}
