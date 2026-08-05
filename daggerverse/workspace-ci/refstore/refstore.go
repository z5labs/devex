// Package refstore records and reads which checks already passed on which
// inputs, as git refs in a GitHub repository.
//
// It exists because the module cannot write the other store. A GitHub Actions
// cache entry is written with ACTIONS_RUNTIME_TOKEN, which only a running
// workflow holds, so recording a pass there has to be a step in the calling
// workflow: one concept split across two places, and no way to record at all
// from a CI system with no equivalent of actions/cache/save. A git ref is
// created with an ordinary repository token, which is the same credential the
// read already takes — so this is a store the module owns both sides of.
//
// An entry is a ref and nothing else; there is no payload to fetch and none is
// ever read:
//
//	refs/workspace-ci-memo/v1/<scope>/<input-hash>/<unix-seconds>
//
// <scope> is the hex encoding of the git ref the recording run was on. Hex
// rather than the ref itself for two reasons, both of which are correctness and
// not taste. A ref name contains slashes, so nesting it raw would make
// refs/heads/main a path prefix of refs/heads/main/feature and a listing of the
// first would return the second's entries — a branch anyone can create could
// then poison the default branch's scope. And a ref name may contain characters
// that are legal in a branch but not in the ref this store would build from it.
// Hex has neither problem, needs no URL escaping, and decodes with
// `printf %s <scope> | xxd -r -p`.
//
// <unix-seconds> is in the name because the TTL is read far more often than it
// is written: GitHub's ref listing carries no timestamp, so a date held anywhere
// else would cost one API call per entry on every plan.
//
// Nothing here imports Dagger, so it is testable against a stub; SelfCheck is
// that test in a form CI runs.
package refstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Namespace is the ref prefix this store owns, and is what a repository ruleset
// should protect if writes are to be restricted to trusted runs. The version in
// it is the escape hatch for a change that must retire every existing entry at
// once, independently of how the input hash itself is composed.
const Namespace = "refs/workspace-ci-memo/v1/"

// pageSize is GitHub's maximum page size, and maxPages bounds the walk: a scope
// larger than that is one where the TTL is doing nothing, and reading it forever
// would cost more time than the skips it buys.
const (
	pageSize = 100
	maxPages = 20
)

// requestTimeout bounds one request. The store is an optimisation, so a slow one
// must not hold up a plan — or a recording — for long.
const requestTimeout = 30 * time.Second

// Scope renders a git ref as the single, opaque path component this store keys
// entries under. See the package doc for why it is hex.
func Scope(ref string) string {
	return hex.EncodeToString([]byte(ref))
}

// prefix is where every entry recorded from ref lives. The trailing slash is
// load-bearing: it is what stops one scope's listing reaching into another whose
// encoded name merely extends it.
func prefix(ref string) string {
	return Namespace + Scope(ref) + "/"
}

// Record writes an entry saying that the leg whose inputs hashed to hash passed
// at commit, under ref's scope.
//
// It reports whether it created anything. An entry for this hash already in this
// scope is left exactly as it is: the entry's whole meaning is its name, so a
// second recording would carry no new information, and refreshing its timestamp
// would extend a TTL whose entire job is to bound how long a pass may be
// honoured.
//
// Errors are the caller's to soften. Recording happens after a check has already
// passed, so a store that will not take the entry must cost a later run some
// time and never fail the run that is holding it.
func Record(ctx context.Context, api, token, repo, ref, hash, commit string, now time.Time) (bool, error) {
	if err := validHash(hash); err != nil {
		return false, err
	}
	if commit == "" {
		return false, fmt.Errorf("recording %s needs the commit that passed: a ref has to point at an object", hash)
	}
	client := &http.Client{Timeout: requestTimeout}

	entry := prefix(ref) + hash + "/"
	existing, err := matching(ctx, client, api, token, repo, strings.TrimSuffix(entry, "/"))
	if err != nil {
		return false, err
	}
	for _, r := range existing {
		if strings.HasPrefix(r, entry) {
			return false, nil
		}
	}

	body, err := json.Marshal(map[string]string{
		"ref": entry + strconv.FormatInt(now.Unix(), 10),
		"sha": commit,
	})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/repos/%s/git/refs", api, repo), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	authorize(req, token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated:
		return true, nil
	case http.StatusUnprocessableEntity:
		// The ref already exists, which here means a concurrent run recorded the
		// same hash in the same second. Both runs proved the same thing, so the
		// loser has nothing to report.
		return false, nil
	default:
		return false, fmt.Errorf("POST %s/git/refs: %s", repo, resp.Status)
	}
}

// Hashes returns the input hashes recorded under ref and written since cutoff.
//
// Entries older than the cutoff are ignored. That TTL is the answer to
// base-image drift, which a source-derived hash cannot see: checks that boot real
// services from floating tags mean an identical tree does not imply an identical
// container. It is applied to the timestamp in the entry's own name, so an entry
// cannot outlive its window by being rewritten.
//
// An error here is soft by contract, exactly as it is for the Actions cache
// store: the caller shrinks its known-good set, which costs CI time and never
// correctness.
func Hashes(ctx context.Context, api, token, repo, ref string, cutoff time.Time) ([]string, error) {
	client := &http.Client{Timeout: requestTimeout}
	p := prefix(ref)
	refs, err := matching(ctx, client, api, token, repo, strings.TrimSuffix(p, "/"))
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var out []string
	for _, r := range refs {
		rest, ok := strings.CutPrefix(r, p)
		if !ok {
			continue // a neighbouring scope whose encoded name extends this one
		}
		hash, stamp, ok := strings.Cut(rest, "/")
		if !ok || hash == "" || strings.Contains(stamp, "/") {
			continue // not an entry this version of the store wrote
		}
		if validHash(hash) != nil {
			continue
		}
		secs, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil {
			continue
		}
		if time.Unix(secs, 0).Before(cutoff) {
			continue
		}
		if seen[hash] {
			continue
		}
		seen[hash] = true
		out = append(out, hash)
	}
	return out, nil
}

// matching lists every ref beginning with the given prefix. GitHub's endpoint
// takes the prefix with "refs/" stripped and matches on whole path components
// only in the sense that it does no matching of its own — it is a plain string
// prefix — which is why every caller here re-filters on a prefix ending in "/".
func matching(ctx context.Context, client *http.Client, api, token, repo, refPrefix string) ([]string, error) {
	var out []string
	for page := 1; page <= maxPages; page++ {
		found, more, err := matchingPage(ctx, client, api, token, repo, refPrefix, page)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
		if !more {
			return out, nil
		}
	}
	return out, fmt.Errorf("stopped reading the store at %q after %d pages", refPrefix, maxPages)
}

func matchingPage(
	ctx context.Context,
	client *http.Client,
	api, token, repo, refPrefix string,
	page int,
) (refs []string, more bool, err error) {
	q := url.Values{
		"per_page": {fmt.Sprint(pageSize)},
		"page":     {fmt.Sprint(page)},
	}
	endpoint := fmt.Sprintf("%s/repos/%s/git/matching-refs/%s?%s",
		api, repo, strings.TrimPrefix(refPrefix, "refs/"), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, err
	}
	authorize(req, token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// GitHub answers a prefix that matches nothing either way depending on how
		// much of it exists. Neither means the store is broken.
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("GET %s/git/matching-refs: %s", repo, resp.Status)
	}
	var listing []struct {
		Ref string `json:"ref"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		return nil, false, fmt.Errorf("decode the ref listing: %w", err)
	}
	for _, e := range listing {
		refs = append(refs, e.Ref)
	}
	return refs, len(listing) >= pageSize, nil
}

func authorize(req *http.Request, token string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// validHash rejects anything that is not an input hash as this planner spells
// them. A hash is folded straight into a ref name, so a value carrying a slash or
// a character git will not accept would either land somewhere it was not meant to
// or fail obscurely at the API.
func validHash(hash string) error {
	if hash == "" {
		return fmt.Errorf("an empty input hash is a leg that must never be memoized")
	}
	for i := 0; i < len(hash); i++ {
		c := hash[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return fmt.Errorf("%q is not an input hash: they are lowercase hex", hash)
	}
	return nil
}
