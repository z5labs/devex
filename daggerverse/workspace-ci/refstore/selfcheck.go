package refstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"
)

// SelfCheck exercises the whole module-owned store — recording an entry,
// declining to record one that is already there, reading entries back, dropping
// ones older than the TTL, and keeping one ref's scope out of another's — against
// an in-process stub of GitHub's git-refs API.
//
// It is here rather than only in refstore_test.go because a consumer's CI runs
// checks, not `go test`, and this is the half of memoization whose failure mode
// is silent: a store that will not take an entry costs nothing today and every
// later run its full time, while a scope that leaks costs correctness. The stub
// binds loopback inside the calling process, so this needs no network, no
// credential and no service.
func SelfCheck() error {
	var errs []error
	for _, c := range []struct {
		name string
		run  func(*stub) error
	}{
		{"records an entry under its ref's scope", checkRecords},
		{"does not record the same hash twice", checkIsIdempotent},
		{"reads recorded hashes back", checkReadsBack},
		{"ignores an entry older than the TTL", checkHonoursTTL},
		{"keeps one scope out of another that extends it", checkScopesAreIsolated},
		{"reports a store it cannot read", checkReportsAnUnreadableStore},
		{"reports a store that refuses a write", checkReportsARefusedWrite},
		{"refuses to key an entry on something that is not a hash", checkRejectsABadHash},
	} {
		s := newStub()
		err := c.run(s)
		s.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.name, err))
		}
	}
	return errors.Join(errs...)
}

const (
	selfRepo   = "z5labs/devex"
	selfToken  = "self-check"
	selfCommit = "0123456789abcdef0123456789abcdef01234567"
	selfHash   = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	selfOther  = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	selfRef    = "refs/heads/main"
	// selfNeighbour is chosen so that Scope(selfRef) is a strict prefix of
	// Scope(selfNeighbour) — which is exactly the leak the trailing slash exists
	// to close, and would be invisible against an unrelated branch name.
	selfNeighbour = "refs/heads/main-2"
)

func checkRecords(s *stub) error {
	now := time.Unix(1_700_000_000, 0)
	created, err := Record(context.Background(), s.URL, selfToken, selfRepo, selfRef, selfHash, selfCommit, now)
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("recording into an empty store created nothing")
	}
	want := Namespace + Scope(selfRef) + "/" + selfHash + "/1700000000"
	if got := s.refs(); !slices.Equal(got, []string{want}) {
		return fmt.Errorf("the store holds %v, want [%s]", got, want)
	}
	if got := s.sha(want); got != selfCommit {
		return fmt.Errorf("the entry points at %q, want the commit that passed (%q)", got, selfCommit)
	}
	return nil
}

func checkIsIdempotent(s *stub) error {
	ctx := context.Background()
	if _, err := Record(ctx, s.URL, selfToken, selfRepo, selfRef, selfHash, selfCommit, time.Unix(1_700_000_000, 0)); err != nil {
		return err
	}
	before := s.writes()
	// A day later, which is what makes this worth asserting: a re-record that
	// refreshed the timestamp would extend a TTL whose job is to bound how long a
	// pass may be honoured.
	created, err := Record(ctx, s.URL, selfToken, selfRepo, selfRef, selfHash, selfCommit, time.Unix(1_700_086_400, 0))
	if err != nil {
		return err
	}
	if created {
		return fmt.Errorf("recording a hash the store already held reported a new entry")
	}
	if got := s.writes(); got != before {
		return fmt.Errorf("recording a hash the store already held issued %d write(s)", got-before)
	}
	if got := len(s.refs()); got != 1 {
		return fmt.Errorf("the store holds %d entries for one hash", got)
	}
	return nil
}

func checkReadsBack(s *stub) error {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	for _, h := range []string{selfHash, selfOther} {
		if _, err := Record(ctx, s.URL, selfToken, selfRepo, selfRef, h, selfCommit, now); err != nil {
			return err
		}
	}
	got, err := Hashes(ctx, s.URL, selfToken, selfRepo, selfRef, now.Add(-time.Hour))
	if err != nil {
		return err
	}
	slices.Sort(got)
	want := []string{selfHash, selfOther}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return fmt.Errorf("read %v back, want %v", got, want)
	}
	return nil
}

func checkHonoursTTL(s *stub) error {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	if _, err := Record(ctx, s.URL, selfToken, selfRepo, selfRef, selfHash, selfCommit, now.Add(-48*time.Hour)); err != nil {
		return err
	}
	if _, err := Record(ctx, s.URL, selfToken, selfRepo, selfRef, selfOther, selfCommit, now.Add(-time.Minute)); err != nil {
		return err
	}
	got, err := Hashes(ctx, s.URL, selfToken, selfRepo, selfRef, now.Add(-24*time.Hour))
	if err != nil {
		return err
	}
	if !slices.Equal(got, []string{selfOther}) {
		return fmt.Errorf("read %v back, want only the entry inside the TTL (%s)", got, selfOther)
	}
	return nil
}

func checkScopesAreIsolated(s *stub) error {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	if !strings.HasPrefix(Scope(selfNeighbour), Scope(selfRef)) {
		return fmt.Errorf("the fixture no longer proves anything: %q does not extend %q", selfNeighbour, selfRef)
	}
	if _, err := Record(ctx, s.URL, selfToken, selfRepo, selfNeighbour, selfHash, selfCommit, now); err != nil {
		return err
	}
	got, err := Hashes(ctx, s.URL, selfToken, selfRepo, selfRef, now.Add(-time.Hour))
	if err != nil {
		return err
	}
	if len(got) != 0 {
		return fmt.Errorf("%q read %v out of %q's scope", selfRef, got, selfNeighbour)
	}
	// And the neighbour must still be able to read its own.
	got, err = Hashes(ctx, s.URL, selfToken, selfRepo, selfNeighbour, now.Add(-time.Hour))
	if err != nil {
		return err
	}
	if !slices.Equal(got, []string{selfHash}) {
		return fmt.Errorf("%q read %v of its own entries", selfNeighbour, got)
	}
	return nil
}

func checkReportsAnUnreadableStore(s *stub) error {
	s.deny(http.StatusForbidden)
	if _, err := Hashes(context.Background(), s.URL, selfToken, selfRepo, selfRef, time.Unix(0, 0)); err == nil {
		return fmt.Errorf("a 403 on the listing was read as an empty store")
	}
	return nil
}

func checkReportsARefusedWrite(s *stub) error {
	s.deny(http.StatusForbidden)
	_, err := Record(context.Background(), s.URL, selfToken, selfRepo, selfRef, selfHash, selfCommit, time.Unix(1_700_000_000, 0))
	if err == nil {
		return fmt.Errorf("a store that refused the write reported success")
	}
	return nil
}

func checkRejectsABadHash(s *stub) error {
	for _, bad := range []string{"", "not-a-hash", selfHash + "/../" + selfOther, strings.ToUpper(selfHash)} {
		if _, err := Record(context.Background(), s.URL, selfToken, selfRepo, selfRef, bad, selfCommit, time.Now()); err == nil {
			return fmt.Errorf("%q was accepted as an input hash", bad)
		}
	}
	if got := s.writes(); got != 0 {
		return fmt.Errorf("a rejected hash still issued %d write(s)", got)
	}
	return nil
}

// stub is enough of GitHub's git-refs API to record against: a map from ref name
// to the object it points at, listed by prefix and created once.
type stub struct {
	*httptest.Server

	mu      sync.Mutex
	objects map[string]string
	written int
	denied  int
}

func newStub() *stub {
	s := &stub{objects: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// deny makes every subsequent request fail with status, which is how a token
// without the scope, or a ruleset protecting the namespace, actually presents.
func (s *stub) deny(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.denied = status
}

func (s *stub) refs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.objects))
	for r := range s.objects {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

func (s *stub) sha(ref string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[ref]
}

func (s *stub) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.denied != 0 {
		w.WriteHeader(s.denied)
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+selfToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	base := "/repos/" + selfRepo + "/git/"
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, base+"matching-refs/"):
		want := "refs/" + strings.TrimPrefix(r.URL.Path, base+"matching-refs/")
		var out []map[string]string
		for _, ref := range slices.Sorted(maps.Keys(s.objects)) {
			if strings.HasPrefix(ref, want) {
				out = append(out, map[string]string{"ref": ref})
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPost && r.URL.Path == base+"refs":
		s.written++
		var body struct{ Ref, Sha string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, ok := s.objects[body.Ref]; ok {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		s.objects[body.Ref] = body.Sha
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}
