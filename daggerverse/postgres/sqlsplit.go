package main

import "strings"

// splitSQL splits a SQL script into individual statements on `;`
// boundaries that fall outside string literals and comments. It
// recognises the lexical constructs that can legitimately contain a
// semicolon:
//
//   - single-quoted strings  '...'   ('' is an escaped quote)
//   - double-quoted idents   "..."   ("" is an escaped quote)
//   - line comments          -- ... \n
//   - block comments         /* ... */ (PostgreSQL nests these)
//   - dollar-quoted strings  $$ ... $$ and $tag$ ... $tag$
//
// Empty statements (whitespace/comment-only) are dropped. The trailing
// statement is emitted even without a closing `;`.
func splitSQL(script string) []string {
	var (
		stmts   []string
		buf     strings.Builder
		i       int
		n       = len(script)
		comment int // block-comment nesting depth
	)

	flush := func() {
		s := strings.TrimSpace(buf.String())
		if s != "" {
			stmts = append(stmts, s)
		}
		buf.Reset()
	}

	for i < n {
		// Inside a (possibly nested) block comment: consume until the
		// matching close, tracking PostgreSQL's nesting.
		if comment > 0 {
			if strings.HasPrefix(script[i:], "/*") {
				comment++
				buf.WriteString("/*")
				i += 2
				continue
			}
			if strings.HasPrefix(script[i:], "*/") {
				comment--
				buf.WriteString("*/")
				i += 2
				continue
			}
			buf.WriteByte(script[i])
			i++
			continue
		}

		ch := script[i]
		switch {
		case strings.HasPrefix(script[i:], "/*"):
			comment++
			buf.WriteString("/*")
			i += 2
		case strings.HasPrefix(script[i:], "--"):
			// Line comment: copy through end-of-line (keep the newline).
			j := strings.IndexByte(script[i:], '\n')
			if j < 0 {
				buf.WriteString(script[i:])
				i = n
			} else {
				buf.WriteString(script[i : i+j+1])
				i += j + 1
			}
		case ch == '\'':
			i = consumeQuoted(script, i, '\'', &buf)
		case ch == '"':
			i = consumeQuoted(script, i, '"', &buf)
		case ch == '$':
			if tag, end, ok := dollarTag(script, i); ok {
				i = consumeDollar(script, tag, end, &buf)
			} else {
				buf.WriteByte(ch)
				i++
			}
		case ch == ';':
			flush()
			i++
		default:
			buf.WriteByte(ch)
			i++
		}
	}
	flush()
	return stmts
}

// consumeQuoted copies a quoted run starting at the opening quote
// `script[start]` (== q) through its closing quote, handling the
// doubled-quote escape (`''` / `""`). Returns the index just past the
// run.
func consumeQuoted(script string, start int, q byte, buf *strings.Builder) int {
	n := len(script)
	buf.WriteByte(q)
	i := start + 1
	for i < n {
		c := script[i]
		if c == q {
			if i+1 < n && script[i+1] == q { // escaped quote
				buf.WriteByte(q)
				buf.WriteByte(q)
				i += 2
				continue
			}
			buf.WriteByte(q)
			return i + 1
		}
		buf.WriteByte(c)
		i++
	}
	return i
}

// dollarTag reports whether script[start] begins a dollar-quote opening
// tag (`$$` or `$ident$`). When ok, it returns the full tag (including
// both `$` delimiters) and the index just past it.
func dollarTag(script string, start int) (tag string, end int, ok bool) {
	n := len(script)
	// start points at the first '$'. Scan an optional identifier then a
	// closing '$'.
	i := start + 1
	for i < n {
		c := script[i]
		if c == '$' {
			return script[start : i+1], i + 1, true
		}
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			i++
			continue
		}
		return "", start, false
	}
	return "", start, false
}

// consumeDollar copies a dollar-quoted string. `tag` is the delimiter
// (e.g. `$$` or `$body$`) and `bodyStart` is the index just past the
// opening tag. Returns the index just past the closing tag.
func consumeDollar(script string, tag string, bodyStart int, buf *strings.Builder) int {
	n := len(script)
	buf.WriteString(tag)
	i := bodyStart
	for i < n {
		if strings.HasPrefix(script[i:], tag) {
			buf.WriteString(tag)
			return i + len(tag)
		}
		buf.WriteByte(script[i])
		i++
	}
	return i
}
