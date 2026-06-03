package skill

import (
	"fmt"
	"regexp"
)

// dbNamePattern constrains the database name: it flows into the generated
// skill's frontmatter `name: pg-<db>` and into filenames, so anything outside
// this set could break the skill or escape a path segment.
var dbNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidateDBName rejects a database name that isn't a safe single path/name
// segment. The parent module calls this before any introspection, so an unsafe
// name aborts before touching the network or producing any output.
func ValidateDBName(db string) error {
	if !dbNamePattern.MatchString(db) {
		return fmt.Errorf("invalid db name %q: must match %s", db, dbNamePattern.String())
	}
	return nil
}
