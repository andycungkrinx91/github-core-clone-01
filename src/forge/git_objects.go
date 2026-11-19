package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type GitObject struct {
	Type string // "commit", "tree", "blob", "tag"
	Data []byte
}

func hashGitObject(objType string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(objType))
	h.Write([]byte{' '})
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func (r *Repository) putObjectLocked(obj GitObject) string {
	oid := hashGitObject(obj.Type, obj.Data)
	if _, exists := r.Objects[oid]; !exists {
		cp := make([]byte, len(obj.Data))
		copy(cp, obj.Data)
		r.Objects[oid] = GitObject{Type: obj.Type, Data: cp}
	}
	return oid
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) ([]TreeEntry, error) {
	entries := make([]TreeEntry, 0)
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil, fmt.Errorf("malformed tree entry: missing space")
		}
		mode := string(data[off : off+sp])
		nameStart := off + sp + 1
		nul := bytes.IndexByte(data[nameStart:], 0)
		if nul < 0 {
			return nil, fmt.Errorf("malformed tree entry: missing NUL")
		}
		name := string(data[nameStart : nameStart+nul])
		oidStart := nameStart + nul + 1
		if oidStart+20 > len(data) {
			return nil, fmt.Errorf("malformed tree entry: truncated oid")
		}
		oid := hex.EncodeToString(data[oidStart : oidStart+20])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		off = oidStart + 20
	}
	return entries, nil
}

func gitTreeSortKey(e TreeEntry) string {
	if e.Mode == "40000" || e.Mode == "040000" {
		return e.Name + "/"
	}
	return e.Name
}

func buildTreeBytes(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return gitTreeSortKey(sorted[i]) < gitTreeSortKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		mode := e.Mode
		if mode == "040000" {
			mode = "40000"
		}
		buf.WriteString(mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		rawOID, _ := hex.DecodeString(e.OID)
		buf.Write(rawOID)
	}
	return buf.Bytes()
}

type ParsedCommit struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) (ParsedCommit, error) {
	var c ParsedCommit
	idx := bytes.Index(data, []byte("\n\n"))
	header := data
	if idx >= 0 {
		header = data[:idx]
		c.Message = string(data[idx+2:])
	}
	for _, line := range strings.Split(string(header), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		k := line[:sp]
		v := line[sp+1:]
		switch k {
		case "tree":
			c.Tree = v
		case "parent":
			c.Parents = append(c.Parents, v)
		case "author":
			c.Author = v
		case "committer":
			c.Committer = v
		}
	}
	if len(c.Tree) != 40 {
		return c, fmt.Errorf("commit missing valid tree")
	}
	return c, nil
}

func buildCommitBytes(treeOID string, parents []string, identity string, timestamp int64, message string) []byte {
	var buf bytes.Buffer
	buf.WriteString("tree ")
	buf.WriteString(treeOID)
	buf.WriteByte('\n')
	for _, p := range parents {
		buf.WriteString("parent ")
		buf.WriteString(p)
		buf.WriteByte('\n')
	}
	ts := strconv.FormatInt(timestamp, 10)
	buf.WriteString("author ")
	buf.WriteString(identity)
	buf.WriteByte(' ')
	buf.WriteString(ts)
	buf.WriteString(" +0000\n")
	buf.WriteString("committer ")
	buf.WriteString(identity)
	buf.WriteByte(' ')
	buf.WriteString(ts)
	buf.WriteString(" +0000\n\n")
	buf.WriteString(message)
	return buf.Bytes()
}

func parseTagTarget(data []byte) (string, string, bool) {
	idx := bytes.Index(data, []byte("\n\n"))
	header := data
	if idx >= 0 {
		header = data[:idx]
	}
	var obj, typ string
	for _, line := range strings.Split(string(header), "\n") {
		if strings.HasPrefix(line, "object ") {
			obj = line[len("object "):]
		} else if strings.HasPrefix(line, "type ") {
			typ = line[len("type "):]
		}
	}
	if len(obj) == 40 {
		return obj, typ, true
	}
	return "", "", false
}

func (r *Repository) peelTagLocked(oid string) (string, bool) {
	cur := oid
	peeled := false
	for depth := 0; depth < 16; depth++ {
		obj, ok := r.Objects[cur]
		if !ok || obj.Type != "tag" {
			break
		}
		target, _, ok := parseTagTarget(obj.Data)
		if !ok {
			break
		}
		cur = target
		peeled = true
	}
	return cur, peeled
}

func (r *Repository) isAncestorLocked(ancestorOID, descendantOID string) bool {
	if ancestorOID == descendantOID {
		return true
	}
	visited := make(map[string]struct{}, 32)
	queue := []string{descendantOID}
	visited[descendantOID] = struct{}{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		obj, ok := r.Objects[cur]
		if !ok || obj.Type != "commit" {
			continue
		}
		c, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		for _, p := range c.Parents {
			if p == ancestorOID {
				return true
			}
			if _, seen := visited[p]; !seen {
				visited[p] = struct{}{}
				queue = append(queue, p)
			}
		}
	}
	return false
}

func (r *Repository) findMergeBaseLocked(a, b string) string {
	if a == b {
		return a
	}
	ancA := make(map[string]struct{}, 32)
	q := []string{a}
	ancA[a] = struct{}{}
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		obj, ok := r.Objects[cur]
		if !ok || obj.Type != "commit" {
			continue
		}
		c, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		for _, p := range c.Parents {
			if _, seen := ancA[p]; !seen {
				ancA[p] = struct{}{}
				q = append(q, p)
			}
		}
	}
	seenB := make(map[string]struct{}, 32)
	q = []string{b}
	seenB[b] = struct{}{}
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		if _, ok := ancA[cur]; ok {
			return cur
		}
		obj, ok := r.Objects[cur]
		if !ok || obj.Type != "commit" {
			continue
		}
		c, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		for _, p := range c.Parents {
			if _, seen := seenB[p]; !seen {
				seenB[p] = struct{}{}
				q = append(q, p)
			}
		}
	}
	return ""
}

// flattenCommitFilesLocked returns map[path]blobOID for a commit.
func (r *Repository) flattenCommitFilesLocked(commitOID string) map[string]string {
	out := make(map[string]string)
	if commitOID == "" {
		return out
	}
	obj, ok := r.Objects[commitOID]
	if !ok || obj.Type != "commit" {
		return out
	}
	c, err := parseCommit(obj.Data)
	if err != nil {
		return out
	}
	r.flattenTreeLocked(c.Tree, "", out)
	return out
}

func (r *Repository) flattenTreeLocked(treeOID, prefix string, out map[string]string) {
	obj, ok := r.Objects[treeOID]
	if !ok || obj.Type != "tree" {
		return
	}
	entries, err := parseTree(obj.Data)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		if e.Mode == "40000" || e.Mode == "040000" {
			r.flattenTreeLocked(e.OID, p, out)
		} else {
			out[p] = e.OID
		}
	}
}

type dirNode struct {
	files map[string]string
	dirs  map[string]*dirNode
}

func newDirNode() *dirNode {
	return &dirNode{
		files: make(map[string]string),
		dirs:  make(map[string]*dirNode),
	}
}

// writeFilesTreeLocked builds nested Git tree objects from map[path]blobOID and returns root tree OID.
func (r *Repository) writeFilesTreeLocked(files map[string]string) string {
	root := newDirNode()
	for p, blobOID := range files {
		clean := strings.Trim(p, "/")
		if clean == "" {
			continue
		}
		parts := strings.Split(clean, "/")
		cur := root
		for _, seg := range parts[:len(parts)-1] {
			child, ok := cur.dirs[seg]
			if !ok {
				child = newDirNode()
				cur.dirs[seg] = child
			}
			cur = child
		}
		cur.files[parts[len(parts)-1]] = blobOID
	}

	var build func(n *dirNode) string
	build = func(n *dirNode) string {
		entries := make([]TreeEntry, 0, len(n.files)+len(n.dirs))
		for name, oid := range n.files {
			entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: oid})
		}
		for name, child := range n.dirs {
			if len(child.files) == 0 && len(child.dirs) == 0 {
				continue
			}
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		data := buildTreeBytes(entries)
		return r.putObjectLocked(GitObject{Type: "tree", Data: data})
	}
	return build(root)
}

func (r *Repository) threeWayMergeLocked(ancestorOID, baseOID, headOID string) (map[string]string, bool) {
	anc := r.flattenCommitFilesLocked(ancestorOID)
	base := r.flattenCommitFilesLocked(baseOID)
	head := r.flattenCommitFilesLocked(headOID)

	allPaths := make(map[string]struct{}, len(anc)+len(base)+len(head))
	for p := range anc {
		allPaths[p] = struct{}{}
	}
	for p := range base {
		allPaths[p] = struct{}{}
	}
	for p := range head {
		allPaths[p] = struct{}{}
	}

	merged := make(map[string]string, len(allPaths))
	for p := range allPaths {
		aOID, inA := anc[p]
		bOID, inB := base[p]
		hOID, inH := head[p]

		if bOID == hOID && inB == inH {
			if inB {
				merged[p] = bOID
			}
			continue
		}
		if bOID == aOID && inB == inA {
			if inH {
				merged[p] = hOID
			}
			continue
		}
		if hOID == aOID && inH == inA {
			if inB {
				merged[p] = bOID
			}
			continue
		}
		// Both base and head diverged on the same path -> conflict.
		return nil, true
	}
	return merged, false
}

type DiffFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

func (r *Repository) diffCommitsLocked(baseOID, headOID string) []DiffFile {
	baseFiles := r.flattenCommitFilesLocked(baseOID)
	headFiles := r.flattenCommitFilesLocked(headOID)

	paths := make([]string, 0, len(baseFiles)+len(headFiles))
	seen := make(map[string]struct{}, len(baseFiles)+len(headFiles))
	for p := range baseFiles {
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	for p := range headFiles {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	out := make([]DiffFile, 0)
	for _, p := range paths {
		bOID, inB := baseFiles[p]
		hOID, inH := headFiles[p]
		if inB && inH && bOID == hOID {
			continue
		}
		if !inB && inH {
			lines := countLines(r.Objects[hOID].Data)
			if lines == 0 {
				lines = 1
			}
			out = append(out, DiffFile{
				Filename:  p,
				Status:    "added",
				Additions: lines,
				Deletions: 0,
				Changes:   lines,
			})
		} else if inB && !inH {
			lines := countLines(r.Objects[bOID].Data)
			if lines == 0 {
				lines = 1
			}
			out = append(out, DiffFile{
				Filename:  p,
				Status:    "removed",
				Additions: 0,
				Deletions: lines,
				Changes:   lines,
			})
		} else {
			oldLines := countLines(r.Objects[bOID].Data)
			newLines := countLines(r.Objects[hOID].Data)
			adds := newLines
			if adds == 0 {
				adds = 1
			}
			dels := oldLines
			out = append(out, DiffFile{
				Filename:  p,
				Status:    "modified",
				Additions: adds,
				Deletions: dels,
				Changes:   adds + dels,
			})
		}
	}
	return out
}

func (r *Repository) walkTreeObjectsLocked(treeOID string, seen map[string]struct{}) {
	if _, ok := seen[treeOID]; ok {
		return
	}
	obj, ok := r.Objects[treeOID]
	if !ok {
		return
	}
	seen[treeOID] = struct{}{}
	if obj.Type != "tree" {
		return
	}
	entries, err := parseTree(obj.Data)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Mode == "40000" || e.Mode == "040000" {
			r.walkTreeObjectsLocked(e.OID, seen)
		} else {
			if _, ok := r.Objects[e.OID]; ok {
				seen[e.OID] = struct{}{}
			}
		}
	}
}
