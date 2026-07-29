package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"dagger/bruno/internal/dagger"
)

const (
	// manifestFile is the file bru looks for to decide it is inside a
	// collection root at all. Its absence is bru's exit 4 — a diagnostic that
	// arrives only after a container has been started, and does not say which
	// file it wanted.
	manifestFile = "bruno.json"

	// environmentsDir holds the per-environment variable files `--env` selects
	// between, by file name without its extension.
	environmentsDir = "environments"

	// collectionSettingsFile and folderSettingsFile are the two .bru files that
	// are not requests: they carry the auth, headers, scripts and variables
	// that apply to a whole collection or a whole folder. Linting them as
	// requests would demand a meta { seq } that neither has.
	collectionSettingsFile = "collection.bru"
	folderSettingsFile     = "folder.bru"

	// rootFolder is the folder key for a request sitting at the collection
	// root, matching what path.Dir returns for a bare file name. seq is unique
	// per folder, so the root needs a key like any other folder.
	rootFolder = "."

	// processEnvPrefix is how a collection reaches an environment variable —
	// the only shape a WithSecretVar secret is readable under, since a secret
	// deliberately never becomes a bru variable.
	processEnvPrefix = "process.env."
)

// credentialWords are the substrings that make a variable name
// credential-shaped. A match is a name, not a value: the rule is that a
// variable called something like apiKey has no business carrying a literal in
// a file that is committed, whatever the literal happens to be.
var credentialWords = []string{"token", "key", "password", "secret"}

var (
	// bruBlockHeader matches a top-level block opener. Anchored at column 0
	// because that is where the Bruno writer puts them, and because it is what
	// lets an indented brace inside a script, an example or a JSON body pass
	// through untouched.
	bruBlockHeader = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_:.-]*)\s*([{\[])\s*$`)

	// bruReference matches one {{...}} interpolation. The body excludes braces
	// so that a nested pair never swallows the rest of the line.
	bruReference = regexp.MustCompile(`\{\{([^{}]*)\}\}`)

	// bruIdentifier is a reference this linter is willing to have an opinion
	// about: a plain variable name. Anything else inside {{ }} — an expression,
	// a dotted path, a function call — is bru's runtime to resolve, and
	// flagging it would be guessing.
	bruIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

	// bruSetVar matches a variable a script creates at runtime. Those names
	// exist in no vars block anywhere, so without this every collection that
	// chains a request onto a previous one's response would be reported as
	// referencing something undefined.
	bruSetVar = regexp.MustCompile(`\bbru\.set(?:Env|GlobalEnv)?Var\s*\(\s*['"]([^'"]+)`)
)

// Lint checks a collection's structure without issuing a single request.
//
// Bruno ships no linter, and the failure modes it leaves open are the
// expensive kind: a {{baseUrl}} that resolves nowhere fails at request time in
// CI rather than at review time, and an API key committed as a plaintext value
// under environments/ is a leak nobody notices.
//
// Findings are folded into the returned error rather than returned as a value,
// following kicad's Drc and Erc: Dagger drops a function's value when it also
// returns a non-nil error, so a (findings, error) signature would hide the
// findings on exactly the path that needs them. Warnings that do not fail the
// call are written to stderr instead, so they are still visible in the run's
// logs.
//
// Everything is evaluated in pure Go over the source tree — no container, per
// the module's runtime-I/O convention — which is also why it is
// +cache="session" rather than "never": the result is a function of the
// directory's contents and nothing else.
//
// Scope limit: this reads the collection's block and reference structure, not
// a full .bru parse. Blocks are recognised by their opener at column 0, which
// is where Bruno's own writer puts them.
//
// +cache="session"
func (c *Collection) Lint(
	ctx context.Context,
	// Treat warnings as failures.
	// +default=false
	failOnWarnings bool,
) error {
	if err := c.validate(); err != nil {
		return err
	}
	parsed, err := c.loadTree(ctx)
	if err != nil {
		return err
	}
	findings := parsed.lint(strings.TrimSuffix(c.Environment, ".bru"), c.VarNames)
	sortFindings(findings)

	var errorCount, warningCount int
	for _, f := range findings {
		if f.Warning {
			warningCount++
			continue
		}
		errorCount++
	}
	if errorCount == 0 && warningCount == 0 {
		return nil
	}
	report := fmt.Sprintf("bru lint found %s:\n%s",
		countFindings(errorCount, warningCount), renderFindings(findings))
	if errorCount > 0 || failOnWarnings {
		return fmt.Errorf("%s", report)
	}
	// A warning that fails nothing still has to reach someone. Lint returns a
	// bare error, so stderr is the only channel left — and module stderr shows
	// up under the function's own span.
	fmt.Fprintln(os.Stderr, report)
	return nil
}

// ------------------------------------------------------------------ findings

// finding is one lint result. File is empty when the finding is about the
// collection rather than any one file, and Line is 0 when it is about a file
// as a whole rather than a line within it.
type finding struct {
	File    string
	Line    int
	Warning bool
	Message string
}

func (f finding) location() string {
	switch {
	case f.File == "":
		return "<collection root>"
	case f.Line > 0:
		return fmt.Sprintf("%s:%d", f.File, f.Line)
	default:
		return f.File
	}
}

// sortFindings orders findings by where they are rather than by severity, so
// that everything about one file reads together. Collection-wide findings sort
// first because they are the ones that explain the rest — a missing bruno.json
// is why the environment could not be resolved either.
func sortFindings(findings []finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Message < b.Message
	})
}

func renderFindings(findings []finding) string {
	var b strings.Builder
	for _, f := range findings {
		level := "error"
		if f.Warning {
			level = "warning"
		}
		fmt.Fprintf(&b, "  %-7s %s: %s\n", level, f.location(), f.Message)
	}
	return strings.TrimRight(b.String(), "\n")
}

func countFindings(errors, warnings int) string {
	switch {
	case warnings == 0:
		return plural(errors, "error")
	case errors == 0:
		return plural(warnings, "warning")
	default:
		return plural(errors, "error") + " and " + plural(warnings, "warning")
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// ---------------------------------------------------------------- .bru shapes

// bruFile is one parsed .bru file: its path within the collection, its source,
// and the top-level blocks found in it.
type bruFile struct {
	Path   string
	Source string
	Blocks []bruBlock
}

// bruBlock is one top-level `name { ... }` or `name [ ... ]` block. Bruno uses
// both: dictionaries for vars, headers and asserts, arrays for the
// name-only lists such as vars:secret.
type bruBlock struct {
	Name  string
	Line  int
	Array bool
	Body  []bruEntry
}

// bruEntry is one line inside a block. Key is empty for a line that carries no
// entry — a blank line, or the continuation of a multi-line value.
type bruEntry struct {
	Line     int
	Key      string
	Value    string
	Disabled bool
}

// parseBru splits a .bru file into its top-level blocks.
//
// It is a structural scan, not a parse of the Bruno grammar: a block runs from
// an opener at column 0 to its matching closer at column 0. That is enough to
// find variable references, meta fields and assertion blocks, and it steps
// over the parts of the grammar with no fixed shape — a JS script, a JSON
// body, an example's nested request/response — because everything inside them
// is indented.
func parseBru(src string) []bruBlock {
	var blocks []bruBlock
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		match := bruBlockHeader.FindStringSubmatch(lines[i])
		if match == nil {
			continue
		}
		block := bruBlock{Name: match[1], Line: i + 1, Array: match[2] == "["}
		closer := "}"
		if block.Array {
			closer = "]"
		}
		for i++; i < len(lines); i++ {
			if strings.TrimRight(lines[i], " \t\r") == closer {
				break
			}
			block.Body = append(block.Body, parseBruEntry(lines[i], i+1, block.Array))
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// parseBruEntry reads one line of a block body. A dictionary line is
// `key: value`; an array line is a bare name. Either may be prefixed with ~,
// which is how Bruno marks an entry as disabled — still committed, so still
// this linter's business.
func parseBruEntry(line string, number int, array bool) bruEntry {
	entry := bruEntry{Line: number}
	text := strings.TrimSpace(line)
	if text == "" {
		return entry
	}
	if strings.HasPrefix(text, "~") {
		entry.Disabled = true
		text = strings.TrimSpace(strings.TrimPrefix(text, "~"))
	}
	if array {
		entry.Key = text
		return entry
	}
	key, value, ok := strings.Cut(text, ":")
	if !ok {
		return entry
	}
	entry.Key = strings.TrimSpace(key)
	entry.Value = strings.TrimSpace(value)
	return entry
}

// block returns the first top-level block with the given name.
func (f *bruFile) block(name string) *bruBlock {
	for i := range f.Blocks {
		if f.Blocks[i].Name == name {
			return &f.Blocks[i]
		}
	}
	return nil
}

// value returns the value of a key within a block.
func (b *bruBlock) value(key string) (string, bool) {
	if b == nil {
		return "", false
	}
	for _, entry := range b.Body {
		if entry.Key == key {
			return entry.Value, true
		}
	}
	return "", false
}

// entries returns the block's lines that actually carry an entry.
func (b *bruBlock) entries() []bruEntry {
	if b == nil {
		return nil
	}
	var out []bruEntry
	for _, entry := range b.Body {
		if entry.Key != "" {
			out = append(out, entry)
		}
	}
	return out
}

// varNames collects every variable a file declares, across all of Bruno's
// spellings: `vars` in an environment, `vars:secret`, and the
// `vars:pre-request` / `vars:post-response` blocks a collection, folder or
// request can carry.
//
// A disabled entry still counts as declared. bru would not resolve it, but
// toggling one off in the Bruno app is a routine thing to do, and turning
// every reference to it into an error would make the reference rule punish
// ordinary editing.
func (f *bruFile) varNames() []string {
	var names []string
	for i := range f.Blocks {
		block := &f.Blocks[i]
		if block.Name != "vars" && !strings.HasPrefix(block.Name, "vars:") {
			continue
		}
		for _, entry := range block.entries() {
			names = append(names, entry.Key)
		}
	}
	return names
}

// ------------------------------------------------------------------- loading

// tree is the collection as the linter sees it: the manifest, and every .bru
// file sorted into the role its path gives it.
type tree struct {
	ManifestFound bool
	Manifest      string

	Environments []*bruFile
	Settings     *bruFile
	Folders      []*bruFile
	Requests     []*bruFile

	// SuppliedVars are the names a WithEnvFile file declares. They are
	// variables the run will have and the committed tree cannot show, so
	// without them a collection that gets its baseUrl from --env-file would be
	// reported as referencing something undefined.
	SuppliedVars []string
}

// loadTree reads the collection out of the engine. Reads are one file at a
// time rather than through a container: a lint that needed the CLI image
// pulled would cost more than the check it performs.
func (c *Collection) loadTree(ctx context.Context) (*tree, error) {
	out := &tree{}

	found, err := c.Source.Exists(ctx, manifestFile)
	if err != nil {
		return nil, fmt.Errorf("Lint: look for %s in the collection: %v", manifestFile, err)
	}
	out.ManifestFound = found
	if found {
		contents, err := c.Source.File(manifestFile).Contents(ctx)
		if err != nil {
			return nil, fmt.Errorf("Lint: read %s: %v", manifestFile, err)
		}
		out.Manifest = contents
	}

	paths, err := c.Source.Glob(ctx, "**/*.bru")
	if err != nil {
		return nil, fmt.Errorf("Lint: list the collection's .bru files: %v", err)
	}
	sort.Strings(paths)
	for _, p := range paths {
		contents, err := c.Source.File(p).Contents(ctx)
		if err != nil {
			return nil, fmt.Errorf("Lint: read %s: %v", p, err)
		}
		file := &bruFile{Path: p, Source: contents, Blocks: parseBru(contents)}
		switch {
		case strings.HasPrefix(p, environmentsDir+"/"):
			out.Environments = append(out.Environments, file)
		case p == collectionSettingsFile:
			out.Settings = file
		case path.Base(p) == folderSettingsFile:
			out.Folders = append(out.Folders, file)
		default:
			out.Requests = append(out.Requests, file)
		}
	}

	if c.EnvFile != nil {
		supplied, err := suppliedVarNames(ctx, c.EnvFile)
		if err != nil {
			return nil, err
		}
		out.SuppliedVars = supplied
	}
	return out, nil
}

// suppliedVarNames reads the variable names out of a WithEnvFile file. bru
// picks its environment parser from the extension, so this does too — the two
// shapes are not interchangeable, which is the same reason WithEnvFile mounts
// the file under the extension it arrived with.
func suppliedVarNames(ctx context.Context, file *dagger.File) ([]string, error) {
	name, err := file.Name(ctx)
	if err != nil {
		return nil, fmt.Errorf("Lint: read the environment file's name: %v", err)
	}
	contents, err := file.Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("Lint: read the environment file %q: %v", name, err)
	}
	if strings.EqualFold(path.Ext(name), ".json") {
		return jsonEnvVarNames(contents), nil
	}
	return (&bruFile{Source: contents, Blocks: parseBru(contents)}).varNames(), nil
}

// jsonEnvVarNames reads the names out of a JSON environment. Bruno writes the
// `variables` array form; a flat object is accepted too, because it is what
// anyone hand-writing one reaches for first.
func jsonEnvVarNames(contents string) []string {
	var structured struct {
		Variables []struct {
			Name string `json:"name"`
		} `json:"variables"`
	}
	if err := json.Unmarshal([]byte(contents), &structured); err == nil && len(structured.Variables) > 0 {
		names := make([]string, 0, len(structured.Variables))
		for _, v := range structured.Variables {
			names = append(names, v.Name)
		}
		return names
	}
	var flat map[string]any
	if err := json.Unmarshal([]byte(contents), &flat); err != nil {
		// A malformed environment file is bru's own error to raise, and it is
		// not one of the rules this function owns. Contributing no names is the
		// honest outcome: whatever the file was meant to declare, it declares
		// nothing this linter can see.
		return nil
	}
	names := make([]string, 0, len(flat))
	for name := range flat {
		names = append(names, name)
	}
	return names
}

// --------------------------------------------------------------------- rules

// lint runs every rule and returns the findings, in no particular order.
func (t *tree) lint(environment string, overrides []string) []finding {
	var findings []finding
	findings = append(findings, t.lintManifest()...)
	findings = append(findings, t.lintEnvironment(environment)...)
	findings = append(findings, t.lintPlaintextSecrets()...)
	findings = append(findings, t.lintMeta()...)
	findings = append(findings, t.lintAssertions()...)
	findings = append(findings, t.lintReferences(environment, overrides)...)
	return findings
}

// lintManifest checks that the collection has a root manifest declaring the
// three fields bru reads out of it. This is bru's exit 4, caught with a
// message that says which file is missing.
func (t *tree) lintManifest() []finding {
	if !t.ManifestFound {
		return []finding{{Message: fmt.Sprintf(
			"%s is missing from the collection root: bru exits 4 (\"not a collection root\") without it",
			manifestFile)}}
	}
	var manifest map[string]any
	if err := json.Unmarshal([]byte(t.Manifest), &manifest); err != nil {
		return []finding{{File: manifestFile, Message: fmt.Sprintf("is not valid JSON: %v", err)}}
	}
	var findings []finding
	for _, field := range []string{"version", "name", "type"} {
		value, present := manifest[field]
		text, isText := value.(string)
		if !present || (isText && strings.TrimSpace(text) == "") {
			findings = append(findings, finding{File: manifestFile, Message: fmt.Sprintf(
				"declares no %q: a collection manifest needs version, name and type", field)})
		}
	}
	if kind, ok := manifest["type"].(string); ok && kind != "" && kind != "collection" {
		findings = append(findings, finding{File: manifestFile, Message: fmt.Sprintf(
			"declares type %q: bru only reads %q", kind, "collection")})
	}
	return findings
}

// lintEnvironment checks that the environment WithEnvironment selected is one
// the collection actually ships. bru spends exit 6 on this, after starting a
// container to find out.
func (t *tree) lintEnvironment(environment string) []finding {
	if environment == "" {
		return nil
	}
	var available []string
	for _, env := range t.Environments {
		name := strings.TrimSuffix(path.Base(env.Path), path.Ext(env.Path))
		if name == environment {
			return nil
		}
		available = append(available, name)
	}
	message := fmt.Sprintf("no environment named %q under %s/", environment, environmentsDir)
	if len(available) == 0 {
		return []finding{{Message: message + ": the collection ships none"}}
	}
	sort.Strings(available)
	return []finding{{Message: fmt.Sprintf("%s: the collection ships %s",
		message, strings.Join(quoteAll(available), ", "))}}
}

// lintPlaintextSecrets checks that no credential-shaped variable carries a
// literal value in a committed environment file. Those belong in a
// `vars:secret` block, which holds names rather than values.
//
// A value that is entirely interpolation — {{process.env.API_TOKEN}} — is not
// a literal and is exactly the shape being asked for, so it passes. A disabled
// entry does not: commenting a credential out does not uncommit it.
func (t *tree) lintPlaintextSecrets() []finding {
	var findings []finding
	for _, env := range t.Environments {
		for i := range env.Blocks {
			block := &env.Blocks[i]
			// vars:secret is the destination, not a violation of the rule, and
			// carries no values to leak in the first place.
			if block.Name != "vars" {
				continue
			}
			for _, entry := range block.entries() {
				if !credentialShaped(entry.Key) || !literalValue(entry.Value) {
					continue
				}
				findings = append(findings, finding{File: env.Path, Line: entry.Line, Message: fmt.Sprintf(
					"%q is credential-shaped and carries a literal value in a committed file: move the name to a vars:secret block, or set the value to {{%s<NAME>}}",
					entry.Key, processEnvPrefix)})
			}
		}
	}
	return findings
}

// credentialShaped reports whether a variable name reads like a credential.
func credentialShaped(name string) bool {
	lower := strings.ToLower(name)
	for _, word := range credentialWords {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// literalValue reports whether a value is a committed constant rather than an
// interpolation. Anything holding a {{...}} is resolved elsewhere, so there is
// nothing committed to leak.
func literalValue(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	return !bruReference.MatchString(value)
}

// lintMeta checks that every request declares meta { name } and a seq that is
// unique within its folder. seq is what orders a run; two requests sharing one
// order arbitrarily, and a request with none is ordered arbitrarily against
// every other.
func (t *tree) lintMeta() []finding {
	var findings []finding
	// seq is scoped to a folder, so the map is keyed by both.
	seen := map[string]map[string]string{}
	for _, request := range t.Requests {
		meta := request.block("meta")
		if meta == nil {
			findings = append(findings, finding{File: request.Path,
				Message: "declares no meta block: every request needs meta { name, seq }"})
			continue
		}
		if name, ok := meta.value("name"); !ok || strings.TrimSpace(name) == "" {
			findings = append(findings, finding{File: request.Path, Line: meta.Line,
				Message: "declares no name in its meta block"})
		}
		seq, ok := meta.value("seq")
		if !ok || strings.TrimSpace(seq) == "" {
			findings = append(findings, finding{File: request.Path, Line: meta.Line,
				Message: "declares no seq in its meta block: seq is what orders the run"})
			continue
		}
		folder := path.Dir(request.Path)
		if seen[folder] == nil {
			seen[folder] = map[string]string{}
		}
		if first, clash := seen[folder][seq]; clash {
			findings = append(findings, finding{File: request.Path, Line: meta.Line, Message: fmt.Sprintf(
				"seq %s is already used by %s in the same folder: seq must be unique per folder",
				seq, path.Base(first))})
			continue
		}
		seen[folder][seq] = request.Path
	}
	return findings
}

// lintAssertions warns about a request that checks nothing. It runs, it
// reports a pass, and it would report a pass against an API returning
// anything at all.
func (t *tree) lintAssertions() []finding {
	var findings []finding
	for _, request := range t.Requests {
		if hasChecks(request) {
			continue
		}
		findings = append(findings, finding{File: request.Path, Warning: true,
			Message: "declares neither tests nor asserts: it passes whatever the API returns"})
	}
	return findings
}

// hasChecks reports whether a request declares anything that can fail. A
// disabled assertion does not count — it is the same as not having written it
// — and neither does an empty tests block.
func hasChecks(request *bruFile) bool {
	for i := range request.Blocks {
		block := &request.Blocks[i]
		switch block.Name {
		case "tests":
			for _, line := range block.Body {
				if strings.TrimSpace(line.Key) != "" || strings.TrimSpace(line.Value) != "" {
					return true
				}
			}
		case "assert", "asserts":
			for _, entry := range block.entries() {
				if !entry.Disabled {
					return true
				}
			}
		}
	}
	return false
}

// lintReferences checks that every {{var}} the collection interpolates
// resolves to something the run will have.
//
// The sources are the ones bru itself resolves from: the collection's own
// vars, the folder's, the request's, the selected environment's, a WithVar
// override, a WithEnvFile file, and process.env. Variables a script creates
// with bru.setVar count too — they exist in no vars block anywhere, and
// without them every chained request would read as broken.
//
// With no environment selected, every file under environments/ contributes.
// The alternative would report {{baseUrl}} as unresolved for the common case
// of linting a collection without committing to one of its environments.
func (t *tree) lintReferences(environment string, overrides []string) []finding {
	global := map[string]bool{}
	addAll(global, overrides)
	addAll(global, t.SuppliedVars)
	if t.Settings != nil {
		addAll(global, t.Settings.varNames())
	}
	for _, env := range t.Environments {
		name := strings.TrimSuffix(path.Base(env.Path), path.Ext(env.Path))
		if environment == "" || name == environment {
			addAll(global, env.varNames())
		}
	}
	// A script can set a variable one request consumes and another produces,
	// in either order, so the names it creates are collected across the whole
	// collection rather than per file.
	for _, file := range t.everyFile() {
		for _, match := range bruSetVar.FindAllStringSubmatch(file.Source, -1) {
			global[match[1]] = true
		}
	}

	// Folder-scoped variables reach the requests beneath that folder and
	// nothing else.
	folderVars := map[string]map[string]bool{}
	for _, folder := range t.Folders {
		scope := path.Dir(folder.Path)
		if folderVars[scope] == nil {
			folderVars[scope] = map[string]bool{}
		}
		addAll(folderVars[scope], folder.varNames())
	}

	var findings []finding
	for _, request := range t.Requests {
		defined := map[string]bool{}
		for name := range global {
			defined[name] = true
		}
		for scope, names := range folderVars {
			if scope == rootFolder || strings.HasPrefix(request.Path, scope+"/") {
				for name := range names {
					defined[name] = true
				}
			}
		}
		addAll(defined, request.varNames())

		for _, ref := range references(request) {
			if defined[ref.Name] {
				continue
			}
			findings = append(findings, finding{File: request.Path, Line: ref.Line, Message: fmt.Sprintf(
				"{{%s}} resolves to nothing: %s", ref.Name, resolutionHint(environment))})
		}
	}
	return findings
}

// resolutionHint says what would satisfy an unresolved reference, so the
// finding is actionable without re-running the lint to find out.
func resolutionHint(environment string) string {
	source := "any environment under " + environmentsDir + "/"
	if environment != "" {
		source = fmt.Sprintf("the selected environment %q", environment)
	}
	return fmt.Sprintf("declare it in %s or in a vars block, pass it with with-var, or read it from %s",
		source, strings.TrimSuffix(processEnvPrefix, "."))
}

// reference is one {{name}} interpolation and the line it sits on.
type reference struct {
	Name string
	Line int
}

// references collects the variable references a request interpolates.
//
// Scripts, tests and docs are skipped. A {{...}} there is not interpolated by
// bru at all — scripts reach variables through bru.getVar, and docs are prose
// — so reading them would invent findings out of documentation.
func references(file *bruFile) []reference {
	var out []reference
	seen := map[string]bool{}
	for i := range file.Blocks {
		block := &file.Blocks[i]
		if block.Name == "docs" || block.Name == "tests" || strings.HasPrefix(block.Name, "script") {
			continue
		}
		for _, entry := range block.Body {
			for _, match := range bruReference.FindAllStringSubmatch(entry.Value, -1) {
				name := strings.TrimSpace(match[1])
				switch {
				case strings.HasPrefix(name, "$"):
					// A Bruno dynamic variable: {{$guid}}, {{$timestamp}}.
					// Always available, never declared.
					continue
				case strings.HasPrefix(name, processEnvPrefix):
					continue
				case !bruIdentifier.MatchString(name):
					// An expression rather than a name. Out of scope, and
					// guessing at it would produce findings nobody can act on.
					continue
				}
				// One finding per variable per file: a baseUrl referenced by
				// six requests is one thing to fix, not six.
				if seen[name] {
					continue
				}
				seen[name] = true
				out = append(out, reference{Name: name, Line: entry.Line})
			}
		}
	}
	return out
}

// everyFile returns every .bru file in the collection, whatever its role.
func (t *tree) everyFile() []*bruFile {
	files := make([]*bruFile, 0, len(t.Environments)+len(t.Folders)+len(t.Requests)+1)
	files = append(files, t.Environments...)
	files = append(files, t.Folders...)
	files = append(files, t.Requests...)
	if t.Settings != nil {
		files = append(files, t.Settings)
	}
	return files
}

func addAll(set map[string]bool, names []string) {
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = true
		}
	}
}

func quoteAll(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, fmt.Sprintf("%q", v))
	}
	return out
}
