package memostore

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

// TestHashes covers the three rules the read applies to what GitHub returns: only
// this store's keys count, the prefix is stripped so what comes back is an input
// hash, and an entry older than the TTL is ignored. Honouring a stale entry is a
// correctness bug rather than a slow one, which is what the TTL exists to bound.
func TestHashes(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("key"); got != KeyPrefix {
			t.Errorf("listed the store with key=%q, want %q", got, KeyPrefix)
		}
		if got := r.URL.Query().Get("ref"); got != "refs/heads/main" {
			t.Errorf("listed the store with ref=%q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("listed the store with Authorization=%q", got)
		}
		fmt.Fprintf(w, `{"actions_caches": [
			{"key": %q, "created_at": %q},
			{"key": %q, "created_at": %q},
			{"key": "unrelated-cache-entry", "created_at": %q},
			{"key": %q, "created_at": %q}
		]}`,
			KeyPrefix+"fresh", now.Add(-time.Minute).Format(time.RFC3339),
			KeyPrefix+"stale", now.Add(-48*time.Hour).Format(time.RFC3339),
			now.Format(time.RFC3339),
			KeyPrefix, now.Format(time.RFC3339),
		)
	}))
	defer srv.Close()

	got, err := Hashes(context.Background(), srv.URL, "token", "z5labs/devex", "refs/heads/main", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"fresh"}; !slices.Equal(got, want) {
		t.Fatalf("read %v from the store, want %v", got, want)
	}
}

// TestHashesReportsAnUnreadableStore pins that a refused listing is reported
// rather than silently read as empty. The caller's job is to shrug it off; that is
// only a decision it can make if it is told.
func TestHashesReportsAnUnreadableStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := Hashes(context.Background(), srv.URL, "token", "z5labs/devex", "refs/heads/main", time.Now()); err == nil {
		t.Fatal("a 403 from the store was not reported")
	}
}
