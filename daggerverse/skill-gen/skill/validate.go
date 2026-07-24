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

// DefaultPsqlImage is the psql container image baked into a generated skill
// when the caller doesn't override it. Keeping it here (rather than only in
// the template) lets both the module's +default and Render agree on one value.
const DefaultPsqlImage = "docker.io/alpine/psql:17.7"

// psqlImagePattern constrains the psql image reference. Unlike host/user/db,
// the image is substituted *raw* into query.sh's `"${PSQL_IMAGE:-<image>}"`
// default word — quoting it would change the byte output for the default value
// — so the safety has to come from the charset instead. This set covers every
// legal OCI reference character (registry[:port]/namespace/name:tag@digest)
// while excluding everything the shell would act on inside double quotes ($,
// backtick, backslash, quotes) as well as `}`, which would otherwise close the
// parameter expansion early, and whitespace, which would split the word.
var psqlImagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)

// ValidatePsqlImage rejects a psql image reference that isn't inert when
// substituted into the generated bash script and .env example. Like
// ValidateDBName it runs before any introspection, so a bad value aborts
// before touching the network or producing any output.
func ValidatePsqlImage(image string) error {
	if !psqlImagePattern.MatchString(image) {
		return fmt.Errorf("invalid psql image %q: must match %s", image, psqlImagePattern.String())
	}
	return nil
}
