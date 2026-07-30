package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"dagger/bruno/internal/dagger"
)

const (
	// requestListingFile is the name the canonical request listing is written
	// under on both sides of the comparison. It is the path that shows up in
	// the patch's own `a/` and `b/` headers, so it is named for what it holds
	// rather than for either side of the diff.
	requestListingFile = "requests"
)

// requestMethodBlocks maps a request's verb block onto the method it stands
// for. In a .bru file the verb is the block's name — `get { url: ... }` — so
// this is also the set of blocks that make a .bru file a request rather than a
// folder or collection settings file.
var requestMethodBlocks = map[string]string{
	"get":     "GET",
	"post":    "POST",
	"put":     "PUT",
	"patch":   "PATCH",
	"delete":  "DELETE",
	"head":    "HEAD",
	"options": "OPTIONS",
	"trace":   "TRACE",
}

var (
	// urlBaseVariable matches the interpolation a generated request's url opens
	// with — `{{baseUrl}}/pets`. It names the server, which is the spec's
	// business and not the request set's, so it comes off before comparing.
	urlBaseVariable = regexp.MustCompile(`^\{\{[^{}]*\}\}`)

	// urlOrigin matches the scheme://host[:port] prefix of a url written out in
	// full, for the collection that hardcodes its target instead of resolving
	// one from an environment.
	urlOrigin = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*://[^/]*`)

	// bracedPathParam matches OpenAPI's `{petId}` spelling of a path parameter.
	// `bru import` rewrites it to Bruno's `:petId`, so the two are the same
	// segment and normalising one onto the other keeps a hand-written request
	// comparable with a generated one.
	bracedPathParam = regexp.MustCompile(`^\{([^{}]+)\}$`)
)

// Drift reports how the committed collection differs from the OpenAPI document
// it was generated from.
//
// A new operation in the spec that nobody added to the collection is a silently
// untested endpoint: nothing fails, because nothing asks. This is the check that
// notices — it regenerates the collection from the document and compares the
// result against what is committed.
//
// The comparison is scoped to the request set — each request's method and path,
// deduplicated and sorted — rather than being a diff of the two trees. A
// generated request carries detail the document never described (the tests and
// assertions that make the collection worth running, the ordering, the scripts),
// and a byte-for-byte comparison would call every one of those drift. Query
// strings are dropped and path parameters are normalised onto Bruno's `:name`
// spelling, so the same endpoint written either way reads as the same endpoint.
//
// It returns the difference rather than failing on it, and never fails on drift
// alone: Dagger drops a function's value when it also returns an error, so a
// gating Drift could not hand back the report that says what drifted. CheckDrift
// is the gate; this is the one that tells you what to fix.
//
// +cache="session"
func (c *Collection) Drift(
	ctx context.Context,
	// The OpenAPI document the collection is generated from. YAML or JSON —
	// bru reads the contents, not the file name.
	spec *dagger.File,
) (string, error) {
	result, err := c.drift(ctx, spec)
	if err != nil {
		return "", err
	}
	return result.report(), nil
}

// CheckDrift fails when the committed collection no longer matches the OpenAPI
// document it was generated from, so a pipeline can hang a check on the two
// staying in step.
//
// The report travels in the error rather than alongside it, following Lint: a
// Dagger function that returns an error forfeits its value, so a (report, error)
// signature would hide the report on the one path that needs it.
//
// +cache="session"
func (c *Collection) CheckDrift(
	ctx context.Context,
	// The OpenAPI document the collection is generated from.
	spec *dagger.File,
) error {
	result, err := c.drift(ctx, spec)
	if err != nil {
		return err
	}
	if !result.Drifted {
		return nil
	}
	return fmt.Errorf("%s", result.report())
}

// driftResult is the outcome of one comparison: whether the two request sets
// differ, the patch describing how, and how many requests the spec accounts
// for.
type driftResult struct {
	Drifted  bool
	Patch    string
	Declared int
}

// report renders the outcome for a human. The clean case names the number of
// requests it accounted for, because a comparison that read no requests at all
// would otherwise "match" every document it was handed.
func (r driftResult) report() string {
	if !r.Drifted {
		return fmt.Sprintf("bru drift: the collection matches the OpenAPI document (%s).",
			plural(r.Declared, "request"))
	}
	return strings.Join([]string{
		"bru drift: the collection does not match the OpenAPI document.",
		fmt.Sprintf("  a/%s is the request set the collection commits; b/%s is the one the document declares.",
			requestListingFile, requestListingFile),
		"  + is an operation the document declares that the collection has no request for;",
		"  - is a request the collection commits that the document does not declare.",
		"",
		r.Patch,
	}, "\n")
}

// drift regenerates the collection from the document and compares the two
// request sets.
//
// The expected set comes from `bru import` rather than from a second reading of
// the OpenAPI document: drift is "what bru would generate from this spec versus
// what is committed", and an independent parser here could disagree with bru
// about a document and report a difference nobody could act on.
//
// The two sets are compared through Directory.Changes and the Changeset it
// returns — IsEmpty is the verdict, AsPatch is the report — so the diff itself is
// the engine's rather than something hand-rolled. What this function owns is
// only the normalisation that decides what goes into the listing.
func (c *Collection) drift(ctx context.Context, spec *dagger.File) (driftResult, error) {
	var out driftResult
	if err := c.validate(); err != nil {
		return out, err
	}

	committed, err := c.loadTree(ctx, "Drift")
	if err != nil {
		return out, err
	}

	// The collection name is stamped into the generated bruno.json, which the
	// request set does not include — so the default is as good as reading the
	// committed manifest to match it.
	generatedDir, err := c.Bruno.Generate(ctx, spec, defaultCollectionName, formatBru)
	if err != nil {
		return out, err
	}
	generated, err := (&Collection{Bruno: c.Bruno, Source: generatedDir}).loadTree(ctx, "Drift")
	if err != nil {
		return out, err
	}

	declared := requestSet(generated)
	out.Declared = len(declared)

	changes := listingDirectory(declared).
		Changes(listingDirectory(requestSet(committed)))
	empty, err := changes.IsEmpty(ctx)
	if err != nil {
		return out, fmt.Errorf("Drift: compare the two request sets: %v", err)
	}
	if empty {
		return out, nil
	}

	patch, err := changes.AsPatch().Contents(ctx)
	if err != nil {
		return out, fmt.Errorf("Drift: read the difference between the two request sets: %v", err)
	}
	out.Drifted = true
	out.Patch = strings.TrimRight(patch, "\n")
	return out, nil
}

// listingDirectory stages a request set as the single file the comparison is
// made over. Both sides are staged the same way, so the patch Changes produces
// is a diff of the two listings and nothing else.
func listingDirectory(routes []route) *dagger.Directory {
	var listing strings.Builder
	for _, r := range routes {
		listing.WriteString(r.String())
		listing.WriteString("\n")
	}
	return dag.Directory().WithNewFile(requestListingFile, listing.String())
}

// route is one request reduced to what the OpenAPI document describes about it.
type route struct {
	Method string
	Path   string
}

func (r route) String() string {
	return r.Method + " " + r.Path
}

// requestSet reduces a collection to its routes: deduplicated, and ordered by
// path so that every method on one endpoint reads together and a change to one
// endpoint stays in one hunk of the patch.
//
// Duplicates are folded because a collection may hold more than one request
// against the same endpoint — the second one asserting what the first set up —
// and neither the spec nor this comparison has anything to say about how many
// there are.
func requestSet(t *tree) []route {
	seen := map[string]bool{}
	var routes []route
	for _, request := range t.Requests {
		r, ok := requestRoute(request)
		if !ok {
			continue
		}
		if seen[r.String()] {
			continue
		}
		seen[r.String()] = true
		routes = append(routes, r)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})
	return routes
}

// requestRoute reads a request's method and path out of its verb block.
//
// A .bru file carrying no verb block contributes nothing: it is not a request
// bru could issue, which is a structural problem Lint owns and not a difference
// against the document.
func requestRoute(request *bruFile) (route, bool) {
	for i := range request.Blocks {
		block := &request.Blocks[i]
		method, ok := requestMethodBlocks[block.Name]
		if !ok {
			continue
		}
		url, _ := block.value("url")
		return route{Method: method, Path: routePath(url)}, true
	}
	return route{}, false
}

// routePath reduces a request's url to the path the document describes.
//
// The server comes off — whether it arrived as the `{{baseUrl}}` a generated
// request interpolates or as a hardcoded origin — because which host the
// collection points at is the environment's business. So does the query string:
// OpenAPI describes query parameters beside the path rather than in it, and
// `bru import` writes them into a params:query block, so a url that carries one
// was hand-edited and is still the same endpoint.
func routePath(url string) string {
	text := strings.TrimSpace(url)
	if cut := strings.IndexAny(text, "?#"); cut >= 0 {
		text = text[:cut]
	}
	if urlBaseVariable.MatchString(text) {
		text = urlBaseVariable.ReplaceAllString(text, "")
	} else {
		text = urlOrigin.ReplaceAllString(text, "")
	}

	segments := strings.Split(text, "/")
	kept := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		if match := bracedPathParam.FindStringSubmatch(segment); match != nil {
			segment = ":" + match[1]
		}
		kept = append(kept, segment)
	}
	return "/" + strings.Join(kept, "/")
}
