package main

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type gqlTokenKind int

const (
	tokEOF gqlTokenKind = iota
	tokName
	tokInt
	tokFloat
	tokString
	tokPunct
	tokSpread
)

type gqlToken struct {
	kind gqlTokenKind
	val  string
}

func lexGraphQL(src string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+2 < n && src[i+1] == '.' && src[i+2] == '.' {
			tokens = append(tokens, gqlToken{kind: tokSpread, val: "..."})
			i += 3
			continue
		}
		if strings.ContainsRune("!$:()={}[]@|", rune(c)) {
			tokens = append(tokens, gqlToken{kind: tokPunct, val: string(c)})
			i++
			continue
		}
		if c == '"' {
			tok, nextIdx, err := lexGraphQLString(src, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, tok)
			i = nextIdx
			continue
		}
		if c == '-' || (c >= '0' && c <= '9') {
			tok, nextIdx := lexGraphQLNumber(src, i)
			tokens = append(tokens, tok)
			i = nextIdx
			continue
		}
		if c == '_' || unicode.IsLetter(rune(c)) {
			start := i
			for i < n && (src[i] == '_' || unicode.IsLetter(rune(src[i])) || unicode.IsDigit(rune(src[i]))) {
				i++
			}
			tokens = append(tokens, gqlToken{kind: tokName, val: src[start:i]})
			continue
		}
		return nil, fmt.Errorf("unexpected character %q in GraphQL document", c)
	}
	tokens = append(tokens, gqlToken{kind: tokEOF})
	return tokens, nil
}

func lexGraphQLString(src string, i int) (gqlToken, int, error) {
	n := len(src)
	if i+2 < n && src[i+1] == '"' && src[i+2] == '"' {
		i += 3
		start := i
		for i+2 < n && !(src[i] == '"' && src[i+1] == '"' && src[i+2] == '"') {
			i++
		}
		if i+2 >= n {
			return gqlToken{}, i, fmt.Errorf("unterminated block string")
		}
		return gqlToken{kind: tokString, val: src[start:i]}, i + 3, nil
	}
	i++
	var sb strings.Builder
	for i < n && src[i] != '"' {
		if src[i] == '\\' && i+1 < n {
			i++
			switch src[i] {
			case '"', '\\', '/':
				sb.WriteByte(src[i])
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'u':
				if i+4 < n {
					if v, err := strconv.ParseInt(src[i+1:i+5], 16, 32); err == nil {
						sb.WriteRune(rune(v))
						i += 4
					}
				}
			default:
				sb.WriteByte(src[i])
			}
			i++
			continue
		}
		sb.WriteByte(src[i])
		i++
	}
	if i >= n {
		return gqlToken{}, i, fmt.Errorf("unterminated string")
	}
	return gqlToken{kind: tokString, val: sb.String()}, i + 1, nil
}

func lexGraphQLNumber(src string, i int) (gqlToken, int) {
	n := len(src)
	start := i
	if src[i] == '-' {
		i++
	}
	for i < n && src[i] >= '0' && src[i] <= '9' {
		i++
	}
	isFloat := false
	if i < n && src[i] == '.' {
		isFloat = true
		i++
		for i < n && src[i] >= '0' && src[i] <= '9' {
			i++
		}
	}
	if i < n && (src[i] == 'e' || src[i] == 'E') {
		isFloat = true
		i++
		if i < n && (src[i] == '+' || src[i] == '-') {
			i++
		}
		for i < n && src[i] >= '0' && src[i] <= '9' {
			i++
		}
	}
	if isFloat {
		return gqlToken{kind: tokFloat, val: src[start:i]}, i
	}
	return gqlToken{kind: tokInt, val: src[start:i]}, i
}

type gqlDocument struct {
	ops       []*gqlOperation
	fragments map[string]*gqlFragmentDef
}

type gqlOperation struct {
	opType    string
	name      string
	varDefs   []gqlVarDef
	selection []gqlSelection
}

type gqlVarDef struct {
	name       string
	hasDefault bool
	defaultVal any
}

type gqlFragmentDef struct {
	name          string
	typeCondition string
	selection     []gqlSelection
}

type gqlSelection struct {
	isInlineFrag  bool
	isFragSpread  bool
	alias         string
	name          string
	typeCondition string
	args          map[string]gqlValue
	selection     []gqlSelection
}

type gqlValue struct {
	kind    string
	strVal  string
	intVal  int
	fltVal  float64
	boolVal bool
	listVal []gqlValue
	objVal  map[string]gqlValue
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() gqlToken {
	if p.pos >= len(p.tokens) {
		return gqlToken{kind: tokEOF}
	}
	return p.tokens[p.pos]
}

func (p *gqlParser) next() gqlToken {
	t := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return t
}

func (p *gqlParser) expectPunct(ch string) error {
	t := p.next()
	if t.kind != tokPunct || t.val != ch {
		return fmt.Errorf("expected %q, got %q", ch, t.val)
	}
	return nil
}

func (p *gqlParser) skipType() error {
	if p.peek().kind == tokPunct && p.peek().val == "[" {
		p.next()
		if err := p.skipType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else if p.peek().kind == tokName {
		p.next()
	} else {
		return fmt.Errorf("expected type name, got %q", p.peek().val)
	}
	if p.peek().kind == tokPunct && p.peek().val == "!" {
		p.next()
	}
	return nil
}

func (p *gqlParser) parseValue() (gqlValue, error) {
	t := p.peek()
	if t.kind == tokPunct {
		switch t.val {
		case "$":
			p.next()
			nt := p.next()
			if nt.kind != tokName {
				return gqlValue{}, fmt.Errorf("expected variable name")
			}
			return gqlValue{kind: "var", strVal: nt.val}, nil
		case "[":
			p.next()
			var list []gqlValue
			for !(p.peek().kind == tokPunct && p.peek().val == "]") {
				if p.peek().kind == tokEOF {
					return gqlValue{}, fmt.Errorf("unterminated list")
				}
				v, err := p.parseValue()
				if err != nil {
					return gqlValue{}, err
				}
				list = append(list, v)
			}
			p.next()
			return gqlValue{kind: "list", listVal: list}, nil
		case "{":
			p.next()
			obj := make(map[string]gqlValue)
			for !(p.peek().kind == tokPunct && p.peek().val == "}") {
				if p.peek().kind == tokEOF {
					return gqlValue{}, fmt.Errorf("unterminated input object")
				}
				kt := p.next()
				if kt.kind != tokName {
					return gqlValue{}, fmt.Errorf("expected input field name, got %q", kt.val)
				}
				if err := p.expectPunct(":"); err != nil {
					return gqlValue{}, err
				}
				v, err := p.parseValue()
				if err != nil {
					return gqlValue{}, err
				}
				obj[kt.val] = v
			}
			p.next()
			return gqlValue{kind: "object", objVal: obj}, nil
		}
	}
	p.next()
	switch t.kind {
	case tokString:
		return gqlValue{kind: "string", strVal: t.val}, nil
	case tokInt:
		iv, _ := strconv.Atoi(t.val)
		return gqlValue{kind: "int", intVal: iv}, nil
	case tokFloat:
		fv, _ := strconv.ParseFloat(t.val, 64)
		return gqlValue{kind: "float", fltVal: fv}, nil
	case tokName:
		switch t.val {
		case "true":
			return gqlValue{kind: "bool", boolVal: true}, nil
		case "false":
			return gqlValue{kind: "bool", boolVal: false}, nil
		case "null":
			return gqlValue{kind: "null"}, nil
		default:
			return gqlValue{kind: "enum", strVal: t.val}, nil
		}
	default:
		return gqlValue{}, fmt.Errorf("unexpected token %q in value", t.val)
	}
}

func (p *gqlParser) parseArguments() (map[string]gqlValue, error) {
	if !(p.peek().kind == tokPunct && p.peek().val == "(") {
		return nil, nil
	}
	p.next()
	args := make(map[string]gqlValue)
	for !(p.peek().kind == tokPunct && p.peek().val == ")") {
		if p.peek().kind == tokEOF {
			return nil, fmt.Errorf("unterminated argument list")
		}
		nt := p.next()
		if nt.kind != tokName {
			return nil, fmt.Errorf("expected argument name, got %q", nt.val)
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[nt.val] = val
	}
	p.next()
	return args, nil
}

func (p *gqlParser) skipDirectives() error {
	for p.peek().kind == tokPunct && p.peek().val == "@" {
		p.next()
		if p.next().kind != tokName {
			return fmt.Errorf("expected directive name")
		}
		if _, err := p.parseArguments(); err != nil {
			return err
		}
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []gqlSelection
	for !(p.peek().kind == tokPunct && p.peek().val == "}") {
		if p.peek().kind == tokEOF {
			return nil, fmt.Errorf("unterminated selection set")
		}
		if p.peek().kind == tokSpread {
			p.next()
			if p.peek().kind == tokName && p.peek().val == "on" {
				p.next()
				tc := p.next()
				if tc.kind != tokName {
					return nil, fmt.Errorf("expected type condition after 'on'")
				}
				if err := p.skipDirectives(); err != nil {
					return nil, err
				}
				sub, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				sels = append(sels, gqlSelection{
					isInlineFrag:  true,
					typeCondition: tc.val,
					selection:     sub,
				})
				continue
			}
			if p.peek().kind == tokPunct && p.peek().val == "{" {
				sub, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				sels = append(sels, gqlSelection{isInlineFrag: true, selection: sub})
				continue
			}
			fn := p.next()
			if fn.kind != tokName {
				return nil, fmt.Errorf("expected fragment name")
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sels = append(sels, gqlSelection{isFragSpread: true, name: fn.val})
			continue
		}

		first := p.next()
		if first.kind != tokName {
			return nil, fmt.Errorf("expected field name, got %q", first.val)
		}
		alias := first.val
		fieldName := first.val
		if p.peek().kind == tokPunct && p.peek().val == ":" {
			p.next()
			nt := p.next()
			if nt.kind != tokName {
				return nil, fmt.Errorf("expected field name after alias")
			}
			fieldName = nt.val
		}
		args, err := p.parseArguments()
		if err != nil {
			return nil, err
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		var sub []gqlSelection
		if p.peek().kind == tokPunct && p.peek().val == "{" {
			sub, err = p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
		}
		sels = append(sels, gqlSelection{
			alias:     alias,
			name:      fieldName,
			args:      args,
			selection: sub,
		})
	}
	p.next()
	return sels, nil
}

func parseGraphQLDocument(src string) (*gqlDocument, error) {
	tokens, err := lexGraphQL(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{tokens: tokens}
	doc := &gqlDocument{fragments: make(map[string]*gqlFragmentDef)}
	for p.peek().kind != tokEOF {
		t := p.peek()
		if t.kind == tokPunct && t.val == "{" {
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.ops = append(doc.ops, &gqlOperation{opType: "query", selection: sel})
			continue
		}
		if t.kind == tokName && (t.val == "query" || t.val == "mutation" || t.val == "subscription") {
			op, err := p.parseOperation()
			if err != nil {
				return nil, err
			}
			doc.ops = append(doc.ops, op)
			continue
		}
		if t.kind == tokName && t.val == "fragment" {
			frag, err := p.parseFragment()
			if err != nil {
				return nil, err
			}
			doc.fragments[frag.name] = frag
			continue
		}
		return nil, fmt.Errorf("unexpected token %q at top level", t.val)
	}
	return doc, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	op := &gqlOperation{opType: p.next().val}
	if p.peek().kind == tokName {
		op.name = p.next().val
	}
	if p.peek().kind == tokPunct && p.peek().val == "(" {
		p.next()
		for !(p.peek().kind == tokPunct && p.peek().val == ")") {
			if err := p.expectPunct("$"); err != nil {
				return nil, err
			}
			vn := p.next()
			if vn.kind != tokName {
				return nil, fmt.Errorf("expected variable name")
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			if err := p.skipType(); err != nil {
				return nil, err
			}
			vd := gqlVarDef{name: vn.val}
			if p.peek().kind == tokPunct && p.peek().val == "=" {
				p.next()
				dv, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				vd.hasDefault = true
				vd.defaultVal = evalValue(dv, nil)
			}
			op.varDefs = append(op.varDefs, vd)
		}
		p.next()
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.selection = sel
	return op, nil
}

func (p *gqlParser) parseFragment() (*gqlFragmentDef, error) {
	p.next()
	fn := p.next()
	if fn.kind != tokName {
		return nil, fmt.Errorf("expected fragment name")
	}
	if on := p.next(); on.kind != tokName || on.val != "on" {
		return nil, fmt.Errorf("expected 'on' in fragment definition")
	}
	tc := p.next()
	if tc.kind != tokName {
		return nil, fmt.Errorf("expected type condition in fragment")
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &gqlFragmentDef{name: fn.val, typeCondition: tc.val, selection: sel}, nil
}

func evalValue(v gqlValue, vars map[string]any) any {
	switch v.kind {
	case "var":
		if vars == nil {
			return nil
		}
		return vars[v.strVal]
	case "string", "enum":
		return v.strVal
	case "int":
		return v.intVal
	case "float":
		return v.fltVal
	case "bool":
		return v.boolVal
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(v.listVal))
		for _, item := range v.listVal {
			out = append(out, evalValue(item, vars))
		}
		return out
	case "object":
		out := make(map[string]any, len(v.objVal))
		for k, item := range v.objVal {
			out[k] = evalValue(item, vars)
		}
		return out
	default:
		return nil
	}
}

func evalArgs(args map[string]gqlValue, vars map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = evalValue(v, vars)
	}
	return out
}

// Introspection schema metadata
type gqlSchemaType struct {
	Kind       string
	Name       string
	Fields     []string
	EnumValues []string
}

var gqlSchemaTypes = map[string]gqlSchemaType{
	"Query": {
		Kind:   "OBJECT",
		Name:   "Query",
		Fields: []string{"viewer", "repository", "node", "__type", "__schema"},
	},
	"Mutation": {
		Kind:   "OBJECT",
		Name:   "Mutation",
		Fields: []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	},
	"User": {
		Kind:   "OBJECT",
		Name:   "User",
		Fields: []string{"id", "login", "name"},
	},
	"Repository": {
		Kind: "OBJECT",
		Name: "Repository",
		Fields: []string{
			"id", "databaseId", "name", "nameWithOwner", "description", "isPrivate",
			"owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled",
			"issue", "issues", "pullRequests", "pullRequest",
		},
	},
	"Issue": {
		Kind:   "OBJECT",
		Name:   "Issue",
		Fields: []string{"id", "number", "title", "state", "body", "createdAt", "updatedAt"},
	},
	"PullRequest": {
		Kind:   "OBJECT",
		Name:   "PullRequest",
		Fields: []string{"id", "number", "title", "body", "headRefName", "baseRefName", "mergeable", "state", "merged", "mergedAt", "files"},
	},
	"PullRequestChangedFile": {
		Kind:   "OBJECT",
		Name:   "PullRequestChangedFile",
		Fields: []string{"path", "additions", "deletions"},
	},
	"IssueConnection": {
		Kind:   "OBJECT",
		Name:   "IssueConnection",
		Fields: []string{"nodes", "pageInfo"},
	},
	"PullRequestConnection": {
		Kind:   "OBJECT",
		Name:   "PullRequestConnection",
		Fields: []string{"nodes", "pageInfo"},
	},
	"PullRequestChangedFileConnection": {
		Kind:   "OBJECT",
		Name:   "PullRequestChangedFileConnection",
		Fields: []string{"nodes", "pageInfo"},
	},
	"PageInfo": {
		Kind:   "OBJECT",
		Name:   "PageInfo",
		Fields: []string{"hasNextPage", "endCursor"},
	},
	"Ref": {
		Kind:   "OBJECT",
		Name:   "Ref",
		Fields: []string{"name"},
	},
	"IssueComment": {
		Kind:   "OBJECT",
		Name:   "IssueComment",
		Fields: []string{"id", "body"},
	},
	"IssueCommentEdge": {
		Kind:   "OBJECT",
		Name:   "IssueCommentEdge",
		Fields: []string{"node"},
	},
	"SearchType": {
		Kind:       "ENUM",
		Name:       "SearchType",
		EnumValues: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
	},
	"IssueState": {
		Kind:       "ENUM",
		Name:       "IssueState",
		EnumValues: []string{"OPEN", "CLOSED"},
	},
	"PullRequestState": {
		Kind:       "ENUM",
		Name:       "PullRequestState",
		EnumValues: []string{"OPEN", "CLOSED", "MERGED"},
	},
	"MergeableState": {
		Kind:       "ENUM",
		Name:       "MergeableState",
		EnumValues: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	},
	"PullRequestMergeMethod": {
		Kind:       "ENUM",
		Name:       "PullRequestMergeMethod",
		EnumValues: []string{"MERGE", "SQUASH", "REBASE"},
	},
	"RepositoryPermission": {
		Kind:       "ENUM",
		Name:       "RepositoryPermission",
		EnumValues: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	},
}
