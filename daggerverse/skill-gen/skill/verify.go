package skill

import (
	"fmt"
	"strings"
)

// placeholderTokens are the literal substitution markers that must never
// survive into generated output. We check for this explicit set (rather than a
// generic `<...>` scan) so legitimate shell redirections in query.sh
// (`done < "$file"`, `< <(compgen …)`) don't read as unsubstituted placeholders.
var placeholderTokens = []string{
	"<dbname>", "<db>", "<skill-dir>", "<host>", "<port>", "<user>",
	"<psql-image>", "<regen-command>", "<top tables>", "<top-table>",
	"<table count>", "<view count>", "<enum count>", "<count>",
	"<schema>", "<table>", "<view>", "<enum_type>",
}

// requiredFiles must always be present in a generated skill (enums.md is
// conditional and checked separately).
var requiredFiles = []string{
	PathSKILL, PathREADME, PathQuery, PathEnvExample,
	PathTables, PathRelationships, PathViews, PathIndexes,
}

// Verify sanity-checks a rendered skill tree before it's handed back to the
// caller: required files present and non-empty, no unsubstituted placeholders
// in the model-facing files, and the SKILL.md frontmatter carries the right
// `name` and is model-invocable (no disable-model-invocation). It reproduces
// the original postgres-skill-creator Step-3 checklist.
func Verify(files map[string]string, db string) error {
	for _, p := range requiredFiles {
		content, ok := files[p]
		if !ok {
			return fmt.Errorf("verify: missing required file %s", p)
		}
		if strings.TrimSpace(content) == "" {
			return fmt.Errorf("verify: %s is empty", p)
		}
	}

	for _, p := range []string{PathSKILL, PathREADME, PathQuery, PathEnvExample} {
		for _, tok := range placeholderTokens {
			if strings.Contains(files[p], tok) {
				return fmt.Errorf("verify: %s contains unsubstituted placeholder %q", p, tok)
			}
		}
	}

	fm, err := frontmatter(files[PathSKILL])
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	wantName := "name: pg-" + db
	if !containsLine(fm, wantName) {
		return fmt.Errorf("verify: SKILL.md frontmatter must declare %q", wantName)
	}
	if strings.Contains(fm, "disable-model-invocation") {
		return fmt.Errorf("verify: SKILL.md must be model-invocable (no disable-model-invocation)")
	}
	return nil
}

// frontmatter returns the YAML frontmatter block (between the leading `---`
// fences) of a markdown document.
func frontmatter(doc string) (string, error) {
	if !strings.HasPrefix(doc, "---\n") {
		return "", fmt.Errorf("SKILL.md has no frontmatter")
	}
	rest := doc[len("---\n"):]
	block, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return "", fmt.Errorf("SKILL.md frontmatter is not terminated")
	}
	return block, nil
}

func containsLine(block, line string) bool {
	for l := range strings.SplitSeq(block, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}
