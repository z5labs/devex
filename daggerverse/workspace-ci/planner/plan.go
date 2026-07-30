package planner

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Entry is one leg of the plan: a unit of work a CI system runs on its own
// runner.
//
// Name is the display name, unique across the plan. Module is the repo-relative
// module to invoke with `-m`, so a leg loads only what it runs rather than the
// whole workspace. Filter is the check pattern to pass to `dagger check`
// (`<module-name>:<check>`), empty to run every check the module has. Hash is the
// input hash a pass may be recorded under, empty when this leg must never be
// memoized. Timeout is the check step's budget in minutes and JobTimeout the
// surrounding job's, both so a CI system needs no arithmetic of its own.
type Entry struct {
	Name       string `json:"name"`
	Module     string `json:"module"`
	Filter     string `json:"filter"`
	Hash       string `json:"hash"`
	Timeout    int    `json:"timeout"`
	JobTimeout int    `json:"jobTimeout"`
}

// JobTimeoutHeadroom is the number of minutes a leg's job budget adds over its
// check-step budget: enough for checkout, engine provisioning and the recording
// step, so the step timeout is what fires on a stuck check rather than the job
// cap, which reports far less about what went wrong.
const JobTimeoutHeadroom = 4

// CheckEntry is a leg that runs a single check.
//
// moduleDir is repo-relative and moduleName is the module's own name from its
// dagger.json; `dagger check` names a check <module-name>:<check>, while the
// display name uses the directory because two modules in a workspace may share a
// name (every tests module in this repo is called "tests").
func CheckEntry(moduleDir, moduleName, checkName string) Entry {
	return Entry{
		Name:   moduleDir + ":" + checkName,
		Module: moduleDir,
		Filter: moduleName + ":" + checkName,
	}
}

// ModuleEntry is a leg that runs every check a module has, without the plan ever
// having loaded it. It is what the run-everything path emits, and what a module
// whose checks could not be enumerated falls back to.
func ModuleEntry(moduleDir string) Entry {
	return Entry{Name: moduleDir, Module: moduleDir}
}

// Timeouts are per-leg step budgets in minutes, keyed by either a leg's display
// name or a module directory (which covers every leg of that module).
type Timeouts map[string]int

// ParseTimeouts reads the JSON object form of Timeouts. Function parameters
// cannot be Go maps, so this is how the override table crosses the module
// boundary.
//
// Malformed JSON, a non-object, or a non-positive budget is an error rather than a
// silent fallback: a typo'd key already fails quietly (the default applies), so
// the one thing left to catch loudly is a table that could not be read at all.
func ParseTimeouts(raw string) (Timeouts, error) {
	if strings.TrimSpace(raw) == "" {
		return Timeouts{}, nil
	}
	var t Timeouts
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, fmt.Errorf("parse timeouts %q: %w", raw, err)
	}
	for k, v := range t {
		if v <= 0 {
			return nil, fmt.Errorf("timeout for %q must be positive, got %d", k, v)
		}
	}
	return t, nil
}

// Apply fills in each entry's Timeout and JobTimeout: the override for its name,
// else the override for its module, else def.
func (t Timeouts) Apply(entries []Entry, def int) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		e.Timeout = def
		if v, ok := t[e.Module]; ok {
			e.Timeout = v
		}
		if v, ok := t[e.Name]; ok {
			e.Timeout = v
		}
		e.JobTimeout = e.Timeout + JobTimeoutHeadroom
		out = append(out, e)
	}
	return out
}

// Sort orders a plan by leg name, so the same change always yields byte-identical
// output whatever order the engine answered in.
func Sort(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
}

// Format is how a plan is serialized.
type Format string

const (
	// FormatJSON is the canonical form: an indented JSON array of legs.
	FormatJSON Format = "JSON"
	// FormatGithubActions is a single-line JSON array, ready to write to
	// GITHUB_OUTPUT and expand with fromJSON as a matrix.
	FormatGithubActions Format = "GITHUB_ACTIONS"
)

// Render serializes a plan. A nil slice would marshal to `null`, which survives a
// workflow's non-empty test and then breaks fromJSON, so an empty plan renders as
// `[]`.
func Render(entries []Entry, format Format) (string, error) {
	if entries == nil {
		entries = []Entry{}
	}
	switch format {
	case FormatGithubActions:
		b, err := json.Marshal(entries)
		if err != nil {
			return "", err
		}
		return string(b), nil
	case FormatJSON, "":
		b, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("unknown format %q; want one of %q, %q", format, FormatJSON, FormatGithubActions)
	}
}
