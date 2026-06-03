package skill

import "strings"

// mdCell renders an arbitrary introspected value safe for a GitHub-flavored
// markdown table cell: pipes are escaped so they don't split the column, and
// any embedded CR/LF/tab is collapsed to a single space so a value never
// breaks the row onto multiple lines. Runs of whitespace collapse to one
// space and the result is trimmed, keeping output deterministic regardless of
// incidental whitespace in defaults or comments.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\t':
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// mdText collapses CR/LF/tab to spaces for inline prose (table/view comments
// rendered outside a table), without escaping pipes.
func mdText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\t':
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
