// Package memostore reads the record of which checks already passed on which
// inputs.
//
// The store is GitHub's Actions cache, used as a set: an entry's whole meaning is
// its key, KeyPrefix followed by a leg's input hash. Nothing here writes — a write
// needs ACTIONS_RUNTIME_TOKEN, which only a running workflow has, so recording a
// pass stays a CI-native step. Keeping the read in its own package keeps it free
// of Dagger and so testable against a stub.
package memostore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// KeyPrefix namespaces the store, and is what a CI system must prepend to a leg's
// hash when it records a pass. The version in it is the escape hatch for a change
// that must retire every existing entry at once, independently of how the hash
// itself is composed.
const KeyPrefix = "workspace-ci-memo-v1-"

// GithubAPI is the default API root.
const GithubAPI = "https://api.github.com"

// pageSize is the Actions cache API's maximum page size, and maxPages bounds the
// walk: a store larger than that is one where the TTL is doing nothing, and reading
// it forever would cost more time than the skips it buys.
const (
	pageSize = 100
	maxPages = 20
)

// requestTimeout bounds one listing request. The store is an optimisation, so a
// slow one must not hold up a plan for long.
const requestTimeout = 30 * time.Second

// entry is the part of a listing entry that matters: the key, which for this store
// *is* the payload, and when it was written, which is what the TTL is measured
// against.
type entry struct {
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
}

type listing struct {
	Caches []entry `json:"actions_caches"`
}

// Hashes returns the input hashes recorded under ref and written since cutoff.
//
// Entries older than the cutoff are ignored. That TTL is the answer to base-image
// drift, which a source-derived hash cannot see: checks that boot real services
// from floating tags mean an identical tree does not imply an identical container.
// Rather than fold resolved image digests into the hash — most of the cost
// memoization is meant to avoid, and the end of the "derived from git object ids"
// property that makes a hash stable across engine releases — the window in which
// upstream drift can hide behind a hit is bounded outright.
//
// An error here is soft by contract: the caller shrinks its known-good set, which
// costs CI time and never correctness.
func Hashes(ctx context.Context, api, token, repo, ref string, cutoff time.Time) ([]string, error) {
	client := &http.Client{Timeout: requestTimeout}
	var out []string
	for page := 1; page <= maxPages; page++ {
		found, more, err := listPage(ctx, client, api, token, repo, ref, page, cutoff)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
		if !more {
			return out, nil
		}
	}
	return out, fmt.Errorf("stopped reading the store for %q after %d pages", ref, maxPages)
}

func listPage(
	ctx context.Context,
	client *http.Client,
	api, token, repo, ref string,
	page int,
	cutoff time.Time,
) (hashes []string, more bool, err error) {
	q := url.Values{
		"key":      {KeyPrefix},
		"ref":      {ref},
		"per_page": {fmt.Sprint(pageSize)},
		"page":     {fmt.Sprint(page)},
	}
	endpoint := fmt.Sprintf("%s/repos/%s/actions/caches?%s", api, repo, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("GET %s/actions/caches: %s", repo, resp.Status)
	}
	var page1 listing
	if err := json.NewDecoder(resp.Body).Decode(&page1); err != nil {
		return nil, false, fmt.Errorf("decode the cache listing: %w", err)
	}

	for _, e := range page1.Caches {
		hash, ok := strings.CutPrefix(e.Key, KeyPrefix)
		if !ok || hash == "" {
			continue // some other cache entry entirely
		}
		if e.CreatedAt.Before(cutoff) {
			continue
		}
		hashes = append(hashes, hash)
	}
	return hashes, len(page1.Caches) >= pageSize, nil
}
