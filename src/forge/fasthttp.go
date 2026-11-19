package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	initialConnBufSize = 4 * 1024
	initialOutBufSize  = 4 * 1024
	maxPoolRetainSize  = 64 * 1024
	maxRawHeaderBytes  = 64 * 1024
	inlineBodyLimit    = 2048
)

type connScratch struct {
	buf []byte
	out []byte
	rec rawResponseRecorder
}

var connScratchPool = sync.Pool{
	New: func() any {
		return &connScratch{
			buf: make([]byte, initialConnBufSize),
			out: make([]byte, 0, initialOutBufSize),
		}
	},
}

func equalFoldASCII(b []byte, lit string) bool {
	if len(b) != len(lit) {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		d := lit[i]
		if c == d {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if d >= 'A' && d <= 'Z' {
			d += 'a' - 'A'
		}
		if c != d {
			return false
		}
	}
	return true
}

var (
	crlfBytes     = []byte("\r\n")
	hdrEndBytes   = []byte("\r\n\r\n")
	resp100Bytes  = []byte("HTTP/1.1 100 Continue\r\n\r\n")
	resp400Bytes  = []byte("HTTP/1.1 400 Bad Request\r\nContent-Type: application/json; charset=utf-8\r\nConnection: close\r\nContent-Length: 25\r\n\r\n{\"message\":\"Bad Request\"}")
	resp413Bytes  = []byte("HTTP/1.1 413 Request Entity Too Large\r\nContent-Type: application/json; charset=utf-8\r\nConnection: close\r\nContent-Length: 30\r\n\r\n{\"message\":\"Entity Too Large\"}")
	resp431Bytes  = []byte("HTTP/1.1 431 Request Header Fields Too Large\r\nContent-Type: application/json; charset=utf-8\r\nConnection: close\r\nContent-Length: 33\r\n\r\n{\"message\":\"Headers Too Large\"}")
	http11Bytes   = []byte("HTTP/1.1")
	http10Bytes   = []byte("HTTP/1.0")
	fastIssuePref = "/api/v3/repos/" + viewerLogin + "/"
	fastGitPref   = "/" + viewerLogin + "/"
)

type rawResponseRecorder struct {
	header   http.Header
	status   int
	zeroCopy bool
	single   []byte
	body     bytes.Buffer
}

func (r *rawResponseRecorder) reset(zeroCopy bool) {
	r.status = 0
	r.zeroCopy = zeroCopy
	r.single = nil
	r.body.Reset()
	clear(r.header)
}

func (r *rawResponseRecorder) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header, 6)
	}
	return r.header
}

func (r *rawResponseRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *rawResponseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.zeroCopy && r.body.Len() == 0 && r.single == nil {
		r.single = p
		return len(p), nil
	}
	if r.single != nil {
		_, _ = r.body.Write(r.single)
		r.single = nil
	}
	return r.body.Write(p)
}

func (r *rawResponseRecorder) bytes() []byte {
	if r.single != nil {
		return r.single
	}
	return r.body.Bytes()
}

type connEntry struct {
	conn net.Conn
	idle atomic.Bool
}

type rawHTTPServer struct {
	store        *ForgeStore
	mu           sync.Mutex
	closed       bool
	shuttingDown atomic.Bool
	ln           net.Listener
	conns        map[*connEntry]struct{}
	wg           sync.WaitGroup
}

func newRawHTTPServer(store *ForgeStore) *rawHTTPServer {
	return &rawHTTPServer{
		store: store,
		conns: make(map[*connEntry]struct{}, 64),
	}
}

func (s *rawHTTPServer) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return http.ErrServerClosed
	}
	s.ln = ln
	s.mu.Unlock()

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return http.ErrServerClosed
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return http.ErrServerClosed
		}
		entry := &connEntry{conn: conn}
		s.conns[entry] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()

		go s.serveConn(entry)
	}
}

func (s *rawHTTPServer) Shutdown(ctx context.Context) error {
	s.shuttingDown.Store(true)
	s.mu.Lock()
	s.closed = true
	ln := s.ln
	active := make([]*connEntry, 0, len(s.conns))
	for e := range s.conns {
		active = append(active, e)
	}
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	now := time.Now()
	for _, e := range active {
		if e.idle.Load() {
			_ = e.conn.SetReadDeadline(now)
		}
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for e := range s.conns {
			_ = e.conn.Close()
		}
		s.mu.Unlock()
		return ctx.Err()
	}
}

type parsedRawHead struct {
	method       string
	target       string
	auth         string
	gitProto     string
	contentLen   int
	closeConn    bool
	needFallback bool
	badRequest   bool
}

func parseASCIIContentLength(v []byte) (int, bool) {
	if len(v) == 0 || len(v) > 12 {
		return 0, false
	}
	n := 0
	for _, b := range v {
		if b < '0' || b > '9' {
			return 0, false
		}
		n = n*10 + int(b-'0')
		if n > maxGitBodyBytes {
			return n, true
		}
	}
	return n, true
}

func headerContainsToken(v []byte, token string) bool {
	s := string(v)
	for len(s) > 0 {
		var part string
		if idx := strings.IndexByte(s, ','); idx >= 0 {
			part = strings.TrimSpace(s[:idx])
			s = s[idx+1:]
		} else {
			part = strings.TrimSpace(s)
			s = ""
		}
		if strings.EqualFold(part, token) {
			return true
		}
	}
	return false
}

var httpTokenTable = [128]bool{
	'!': true, '#': true, '$': true, '%': true, '&': true, '\'': true, '*': true,
	'+': true, '-': true, '.': true, '^': true, '_': true, '`': true, '|': true, '~': true,
	'0': true, '1': true, '2': true, '3': true, '4': true, '5': true, '6': true, '7': true, '8': true, '9': true,
	'A': true, 'B': true, 'C': true, 'D': true, 'E': true, 'F': true, 'G': true, 'H': true, 'I': true, 'J': true,
	'K': true, 'L': true, 'M': true, 'N': true, 'O': true, 'P': true, 'Q': true, 'R': true, 'S': true, 'T': true,
	'U': true, 'V': true, 'W': true, 'X': true, 'Y': true, 'Z': true,
	'a': true, 'b': true, 'c': true, 'd': true, 'e': true, 'f': true, 'g': true, 'h': true, 'i': true, 'j': true,
	'k': true, 'l': true, 'm': true, 'n': true, 'o': true, 'p': true, 'q': true, 'r': true, 's': true, 't': true,
	'u': true, 'v': true, 'w': true, 'x': true, 'y': true, 'z': true,
}

func isHTTPToken(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c >= 128 || !httpTokenTable[c] {
			return false
		}
	}
	return true
}

func hasBareCRLF(b []byte) bool {
	return bytes.IndexByte(b, '\r') >= 0 || bytes.IndexByte(b, '\n') >= 0
}

type rawHeadState struct {
	sawHost  bool
	sawCL    bool
	sawClose bool
	sawAuth  bool
	sawProto bool
}

func applyRawHeader(p *parsedRawHead, k, v []byte, st *rawHeadState) bool {
	switch len(k) {
	case 4:
		if !equalFoldASCII(k, "Host") {
			return true
		}
		if st.sawHost {
			p.badRequest = true
			return false
		}
		st.sawHost = true
		return true
	case 6:
		return !equalFoldASCII(k, "Expect")
	case 7:
		return !equalFoldASCII(k, "Upgrade")
	case 10:
		if !equalFoldASCII(k, "Connection") {
			return true
		}
		if headerContainsToken(v, "upgrade") {
			return false
		}
		if headerContainsToken(v, "close") {
			st.sawClose = true
			p.closeConn = true
		} else if !st.sawClose && p.closeConn && headerContainsToken(v, "keep-alive") {
			p.closeConn = false
		}
		return true
	case 12:
		if !equalFoldASCII(k, "Git-Protocol") {
			return true
		}
		if st.sawProto {
			return false
		}
		st.sawProto = true
		p.gitProto = string(v)
		return true
	case 13:
		if !equalFoldASCII(k, "Authorization") {
			return true
		}
		if st.sawAuth {
			return false
		}
		st.sawAuth = true
		p.auth = string(v)
		return true
	case 14:
		if !equalFoldASCII(k, "Content-Length") {
			return true
		}
		if st.sawCL {
			return false
		}
		cl, ok := parseASCIIContentLength(v)
		if !ok {
			return false
		}
		st.sawCL = true
		p.contentLen = cl
		return true
	case 17:
		return !equalFoldASCII(k, "Transfer-Encoding")
	default:
		return true
	}
}

func parseRawHTTPHead(hdrBlock []byte) parsedRawHead {
	var p parsedRawHead
	lineEnd := bytes.Index(hdrBlock, crlfBytes)
	var reqLine []byte
	var pos int
	if lineEnd < 0 {
		reqLine = hdrBlock
		pos = len(hdrBlock)
	} else {
		reqLine = hdrBlock[:lineEnd]
		pos = lineEnd + 2
	}
	if hasBareCRLF(reqLine) {
		p.badRequest = true
		return p
	}

	sp1 := bytes.IndexByte(reqLine, ' ')
	if sp1 <= 0 {
		p.needFallback = true
		return p
	}
	rest := reqLine[sp1+1:]
	sp2 := bytes.IndexByte(rest, ' ')
	if sp2 <= 0 {
		p.needFallback = true
		return p
	}
	proto := rest[sp2+1:]
	isHTTP11 := false
	switch {
	case bytes.Equal(proto, http11Bytes):
		isHTTP11 = true
		p.closeConn = false
	case bytes.Equal(proto, http10Bytes):
		p.closeConn = true
	default:
		p.needFallback = true
		return p
	}

	targetBytes := rest[:sp2]
	if len(targetBytes) == 0 || targetBytes[0] != '/' {
		p.needFallback = true
		return p
	}
	p.method = string(reqLine[:sp1])
	p.target = string(targetBytes)

	var st rawHeadState
	for pos < len(hdrBlock) {
		next := bytes.Index(hdrBlock[pos:], crlfBytes)
		var line []byte
		if next < 0 {
			line = hdrBlock[pos:]
			pos = len(hdrBlock)
		} else {
			line = hdrBlock[pos : pos+next]
			pos += next + 2
		}
		if len(line) == 0 || hasBareCRLF(line) {
			p.badRequest = true
			return p
		}
		if line[0] == ' ' || line[0] == '\t' {
			p.needFallback = true
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 || !isHTTPToken(line[:colon]) {
			p.badRequest = true
			return p
		}
		v := bytes.TrimSpace(line[colon+1:])
		if !applyRawHeader(&p, line[:colon], v, &st) {
			if p.badRequest {
				return p
			}
			p.needFallback = true
		}
	}
	if isHTTP11 && !st.sawHost {
		p.badRequest = true
	}
	return p
}

type fastRespSpec struct {
	statusLine  string
	contentType string
	extraHdr    string
	reqID       string
	closeConn   bool
}

func appendFastHTTPResponse(dst []byte, spec fastRespSpec, body []byte) ([]byte, []byte) {
	dst = append(dst, spec.statusLine...)
	dst = append(dst, "\r\nContent-Type: "...)
	dst = append(dst, spec.contentType...)
	if spec.extraHdr != "" {
		dst = append(dst, "\r\n"...)
		dst = append(dst, spec.extraHdr...)
	}
	if spec.reqID != "" {
		dst = append(dst, "\r\nX-Github-Request-Id: "...)
		dst = append(dst, spec.reqID...)
	}
	if spec.closeConn {
		dst = append(dst, "\r\nConnection: close"...)
	}
	dst = append(dst, "\r\nContent-Length: "...)
	dst = strconv.AppendInt(dst, int64(len(body)), 10)
	dst = append(dst, "\r\n\r\n"...)
	if len(body) <= inlineBodyLimit {
		return append(dst, body...), nil
	}
	return dst, body
}

func appendRecorderHTTPResponse(dst []byte, method string, rec *rawResponseRecorder, closeConn bool) ([]byte, []byte) {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	dst = append(dst, "HTTP/1.1 "...)
	dst = strconv.AppendInt(dst, int64(status), 10)
	dst = append(dst, ' ')
	if text := http.StatusText(status); text != "" {
		dst = append(dst, text...)
	} else {
		dst = append(dst, "Status"...)
	}
	dst = append(dst, "\r\n"...)

	noBody := status == http.StatusNoContent || status == http.StatusNotModified || (status >= 100 && status < 200)
	hasCL := false
	hasConn := false
	for k, vals := range rec.header {
		if strings.EqualFold(k, "Date") && len(vals) == 0 {
			continue
		}
		if strings.EqualFold(k, "Content-Length") {
			hasCL = true
		}
		if strings.EqualFold(k, "Connection") {
			hasConn = true
		}
		for _, v := range vals {
			dst = append(dst, k...)
			dst = append(dst, ": "...)
			dst = append(dst, v...)
			dst = append(dst, "\r\n"...)
		}
	}
	if closeConn && !hasConn {
		dst = append(dst, "Connection: close\r\n"...)
	}
	bodyBytes := rec.bytes()
	if !noBody && !hasCL {
		dst = append(dst, "Content-Length: "...)
		dst = strconv.AppendInt(dst, int64(len(bodyBytes)), 10)
		dst = append(dst, "\r\n"...)
	}
	dst = append(dst, "\r\n"...)
	if noBody || method == http.MethodHead || len(bodyBytes) == 0 {
		return dst, nil
	}
	if len(bodyBytes) <= inlineBodyLimit {
		return append(dst, bodyBytes...), nil
	}
	return dst, bodyBytes
}

func writeHTTPResp(conn net.Conn, hdr, body []byte) error {
	if len(body) == 0 {
		_, err := conn.Write(hdr)
		return err
	}
	bufs := net.Buffers{hdr, body}
	_, err := bufs.WriteTo(conn)
	return err
}

func (s *rawHTTPServer) serveConn(entry *connEntry) {
	conn := entry.conn
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		delete(s.conns, entry)
		s.mu.Unlock()
		s.wg.Done()
	}()

	cs := connScratchPool.Get().(*connScratch)
	buf := cs.buf
	out := cs.out[:0]
	rec := &cs.rec
	defer func() {
		if cap(buf) <= maxPoolRetainSize {
			cs.buf = buf[:cap(buf)]
		} else {
			cs.buf = make([]byte, initialConnBufSize)
		}
		if cap(out) <= maxPoolRetainSize {
			cs.out = out[:0]
		} else {
			cs.out = make([]byte, 0, initialOutBufSize)
		}
		rec.reset(false)
		if rec.body.Cap() > maxPoolRetainSize {
			rec.body = bytes.Buffer{}
		}
		connScratchPool.Put(cs)
	}()

	n := 0

	for {
		hdrEnd := bytes.Index(buf[:n], hdrEndBytes)
		for hdrEnd < 0 {
			if n >= maxRawHeaderBytes {
				_, _ = conn.Write(resp431Bytes)
				return
			}
			if n == len(buf) {
				nextSize := len(buf) * 2
				if nextSize > maxRawHeaderBytes {
					nextSize = maxRawHeaderBytes
				}
				nb := make([]byte, nextSize)
				copy(nb, buf[:n])
				buf = nb
			}
			if n == 0 {
				entry.idle.Store(true)
				if s.shuttingDown.Load() {
					return
				}
			}
			r, err := conn.Read(buf[n:])
			entry.idle.Store(false)
			if err != nil {
				return
			}
			n += r
			hdrEnd = bytes.Index(buf[:n], hdrEndBytes)
		}
		if hdrEnd > maxRawHeaderBytes {
			_, _ = conn.Write(resp431Bytes)
			return
		}

		head := parseRawHTTPHead(buf[:hdrEnd])
		if head.badRequest {
			_, _ = conn.Write(resp400Bytes)
			return
		}
		if head.needFallback {
			s.serveStreamFallback(conn, buf[:n], rec, &out)
			return
		}
		if head.contentLen > maxGitBodyBytes {
			_, _ = conn.Write(resp413Bytes)
			return
		}
		if s.shuttingDown.Load() {
			head.closeConn = true
		}

		bodyStart := hdrEnd + 4
		totalNeed := bodyStart + head.contentLen
		if totalNeed > len(buf) {
			nb := make([]byte, totalNeed)
			copy(nb, buf[:n])
			buf = nb
		}
		for n < totalNeed {
			r, err := conn.Read(buf[n:])
			if err != nil {
				return
			}
			n += r
		}

		reqBody := buf[bodyStart:totalNeed]
		out = out[:0]
		respBody, ok := s.tryFastDispatch(head, reqBody, rec, &out)
		if !ok {
			respBody, ok = s.serveBufferedFallback(conn, buf[:totalNeed], head.closeConn, rec, &out)
			if !ok {
				return
			}
		}
		err := writeHTTPResp(conn, out, respBody)
		rec.single = nil
		if rec.body.Cap() > maxPoolRetainSize {
			rec.body = bytes.Buffer{}
		}
		if err != nil {
			return
		}

		if totalNeed < n {
			copy(buf, buf[totalNeed:n])
			n -= totalNeed
		} else {
			n = 0
			if len(buf) > maxPoolRetainSize {
				buf = make([]byte, initialConnBufSize)
			}
		}
		if head.closeConn || s.shuttingDown.Load() {
			return
		}
	}
}

func (s *rawHTTPServer) serveBufferedFallback(conn net.Conn, rawReq []byte, closeConn bool, rec *rawResponseRecorder, out *[]byte) ([]byte, bool) {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(rawReq)))
	if err != nil {
		_, _ = conn.Write(resp400Bytes)
		return nil, false
	}
	req.RemoteAddr = conn.RemoteAddr().String()
	rec.reset(false)
	s.store.ServeHTTP(rec, req)
	var body []byte
	*out, body = appendRecorderHTTPResponse(*out, req.Method, rec, closeConn)
	return body, true
}

func (s *rawHTTPServer) serveStreamFallback(conn net.Conn, buffered []byte, rec *rawResponseRecorder, out *[]byte) {
	br := bufio.NewReader(io.MultiReader(bytes.NewReader(buffered), conn))
	req, err := http.ReadRequest(br)
	if err != nil {
		_, _ = conn.Write(resp400Bytes)
		return
	}
	req.RemoteAddr = conn.RemoteAddr().String()
	if strings.EqualFold(req.Header.Get("Expect"), "100-continue") {
		if _, err := conn.Write(resp100Bytes); err != nil {
			return
		}
	}
	rec.reset(false)
	s.store.ServeHTTP(rec, req)
	*out = (*out)[:0]
	var body []byte
	*out, body = appendRecorderHTTPResponse(*out, req.Method, rec, true)
	_ = writeHTTPResp(conn, *out, body)
}

func (s *rawHTTPServer) tryFastDispatch(head parsedRawHead, body []byte, rec *rawResponseRecorder, out *[]byte) ([]byte, bool) {
	if !s.store.CheckAuth(head.auth) {
		return nil, false
	}
	if strings.HasPrefix(head.target, fastIssuePref) {
		return s.tryFastIssueRoute(head, body, out)
	}
	if strings.HasPrefix(head.target, fastGitPref) {
		return s.tryFastGitRoute(head, body, rec, out)
	}
	return nil, false
}

func (s *rawHTTPServer) tryFastIssueRoute(head parsedRawHead, body []byte, out *[]byte) ([]byte, bool) {
	tail := head.target[len(fastIssuePref):]
	slash := strings.IndexByte(tail, '/')
	if slash <= 0 {
		return nil, false
	}
	repoName := tail[:slash]
	sub := tail[slash+1:]

	if head.method == http.MethodGet && strings.HasPrefix(sub, "issues/") {
		numStr := sub[len("issues/"):]
		if strings.IndexByte(numStr, '/') >= 0 || strings.IndexByte(numStr, '?') >= 0 {
			return nil, false
		}
		num, err := strconv.Atoi(numStr)
		if err != nil || num <= 0 {
			return nil, false
		}
		repo := s.store.GetRepo(viewerLogin, repoName)
		if repo == nil {
			return nil, false
		}
		repo.mu.RLock()
		iss, ok := repo.Issues[num]
		var payload []byte
		if ok {
			payload = iss.cachedJSON
		}
		repo.mu.RUnlock()
		if !ok {
			return nil, false
		}
		var extBody []byte
		*out, extBody = appendFastHTTPResponse(*out, fastRespSpec{
			statusLine:  "HTTP/1.1 200 OK",
			contentType: "application/json; charset=utf-8",
			reqID:       s.store.NextRequestID(),
			closeConn:   head.closeConn,
		}, payload)
		return extBody, true
	}

	if head.method == http.MethodPost && sub == "issues" && len(body) <= maxJSONBodyBytes {
		repo := s.store.GetRepo(viewerLogin, repoName)
		if repo == nil {
			return nil, false
		}
		var req struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		if len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				return nil, false
			}
		}
		iss, err := s.store.CreateIssue(repo, req.Title, req.Body, req.Labels)
		if err != nil {
			errPayload, _ := json.Marshal(map[string]string{"message": err.Error()})
			var extBody []byte
			*out, extBody = appendFastHTTPResponse(*out, fastRespSpec{
				statusLine:  "HTTP/1.1 422 Unprocessable Entity",
				contentType: "application/json; charset=utf-8",
				reqID:       s.store.NextRequestID(),
				closeConn:   head.closeConn,
			}, errPayload)
			return extBody, true
		}
		repo.mu.RLock()
		payload := iss.cachedJSON
		repo.mu.RUnlock()
		var extBody []byte
		*out, extBody = appendFastHTTPResponse(*out, fastRespSpec{
			statusLine:  "HTTP/1.1 201 Created",
			contentType: "application/json; charset=utf-8",
			reqID:       s.store.NextRequestID(),
			closeConn:   head.closeConn,
		}, payload)
		return extBody, true
	}

	return nil, false
}

func (s *rawHTTPServer) tryFastGitRoute(head parsedRawHead, body []byte, rec *rawResponseRecorder, out *[]byte) ([]byte, bool) {
	tail := head.target[len(fastGitPref):]
	slash := strings.IndexByte(tail, '/')
	if slash <= 4 || !strings.HasSuffix(tail[:slash], ".git") {
		return nil, false
	}
	repoName := tail[:slash-4]
	action := tail[slash+1:]
	repo := s.store.GetRepo(viewerLogin, repoName)
	if repo == nil {
		return nil, false
	}

	if head.method == http.MethodGet && strings.HasPrefix(action, "info/refs?service=") {
		svc := action[len("info/refs?service="):]
		if svc == "git-upload-pack" && strings.Contains(head.gitProto, "version=2") {
			var extBody []byte
			*out, extBody = appendFastHTTPResponse(*out, fastRespSpec{
				statusLine:  "HTTP/1.1 200 OK",
				contentType: "application/x-git-upload-pack-advertisement",
				extraHdr:    "Cache-Control: no-cache",
				closeConn:   head.closeConn,
			}, uploadPackV2AdvBytes)
			return extBody, true
		}
		if svc == "git-receive-pack" {
			repo.mu.RLock()
			cached := repo.recvAdvCache
			repo.mu.RUnlock()
			if cached == nil {
				repo.mu.Lock()
				if repo.recvAdvCache == nil {
					repo.recvAdvCache = buildV0AdvertisementLocked(repo, "git-receive-pack")
				}
				cached = repo.recvAdvCache
				repo.mu.Unlock()
			}
			var extBody []byte
			*out, extBody = appendFastHTTPResponse(*out, fastRespSpec{
				statusLine:  "HTTP/1.1 200 OK",
				contentType: "application/x-git-receive-pack-advertisement",
				extraHdr:    "Cache-Control: no-cache",
				closeConn:   head.closeConn,
			}, cached)
			return extBody, true
		}
		return nil, false
	}

	if head.method == http.MethodPost && action == "git-upload-pack" && strings.Contains(head.gitProto, "version=2") {
		items, _, err := parsePktStream(body, false)
		if err != nil {
			return nil, false
		}
		rec.reset(true)
		s.store.handleUploadPackV2(rec, repo, items)
		var extBody []byte
		if rec.status == 0 || rec.status == http.StatusOK {
			*out, extBody = appendFastHTTPResponse(*out, fastRespSpec{
				statusLine:  "HTTP/1.1 200 OK",
				contentType: "application/x-git-upload-pack-result",
				extraHdr:    "Cache-Control: no-cache",
				closeConn:   head.closeConn,
			}, rec.bytes())
		} else {
			*out, extBody = appendRecorderHTTPResponse(*out, http.MethodPost, rec, head.closeConn)
		}
		return extBody, true
	}

	if head.method == http.MethodPost && action == "git-receive-pack" {
		rec.reset(true)
		req := http.Request{
			Method: http.MethodPost,
			Body:   io.NopCloser(bytes.NewReader(body)),
		}
		s.store.handleGitReceivePack(rec, &req, repo)
		var extBody []byte
		if rec.status == 0 || rec.status == http.StatusOK {
			*out, extBody = appendFastHTTPResponse(*out, fastRespSpec{
				statusLine:  "HTTP/1.1 200 OK",
				contentType: "application/x-git-receive-pack-result",
				extraHdr:    "Cache-Control: no-cache",
				closeConn:   head.closeConn,
			}, rec.bytes())
		} else {
			*out, extBody = appendRecorderHTTPResponse(*out, http.MethodPost, rec, head.closeConn)
		}
		return extBody, true
	}

	return nil, false
}
