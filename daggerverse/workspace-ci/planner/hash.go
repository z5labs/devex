package planner

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"io"
	"slices"
	"sort"
)

// hashVersion namespaces every digest produced here. Bump it whenever the hash
// composition changes in a way that must invalidate previously recorded passes;
// old entries then simply stop matching rather than being silently reinterpreted.
const hashVersion = "z5labs/devex workspace-ci input-hash v1"

// Hasher digests everything that determines what a check computes, so two runs
// agree on a check's hash exactly when nothing that check can read has changed
// between them — which is what makes a recorded pass reusable (#238).
//
// Every digest is built from **git blob object ids**, never Dagger digests.
// Dagger's Directory.digest is explicitly not stable across engine releases, so a
// persisted set keyed on it would be invalidated wholesale by every engine bump;
// git object ids are content hashes git itself already maintains. An engine bump
// still perturbs every hash, correctly — engineVersion is a field in every
// module's dagger.json, and each dagger.json is in its own module's source
// context.
type Hasher struct {
	srcs         map[string]map[string]bool
	blobs        map[string]string
	reattributed map[string][]string
	global       string
	memo         map[string]string
}

// NewHasher builds a Hasher, or reports ok=false when even the global inputs are
// unhashable — in which case nothing may be memoized.
//
//   - rootClosure is the root module's transitive dependency closure, whose
//     source contexts together are the global inputs. It is the closure and not
//     the root module's own context because the machinery that selects, routes
//     and records checks is free to live in a dependency: once workspace-ci
//     itself is that dependency, hashing only the root's own sources would leave
//     the planner outside its own trust boundary.
//   - srcs maps a module directory to the repo-relative paths in its Dagger
//     source context, i.e. precisely what Dagger uploads for it (the same
//     source-context notion Attribute narrows with). It must cover rootClosure.
//   - blobs maps a repo-relative path to its git blob object id at HEAD.
//   - bindings is the aggregator-binding reattribution map from
//     AggregatorBindings, applied here for the same reason Attribute applies it
//     (#179): a per-toolchain binding is provably owned by one toolchain, and
//     repo convention requires regenerating it whenever that toolchain's sources
//     shift line numbers. Folding it into the global inputs instead would make
//     the routine binding refresh that accompanies almost every module change
//     perturb every check's hash, and memoization would essentially never hit.
//   - globalPaths and nonGlobal are the extra global inputs and the subtractions
//     from them; see GlobalPathsDefault and NonGlobalRootPaths.
func NewHasher(
	rootClosure map[string]bool,
	srcs map[string]map[string]bool,
	blobs map[string]string,
	bindings map[string]string,
	globalPaths []string,
	nonGlobal []string,
) (*Hasher, bool) {
	h := &Hasher{
		srcs:         srcs,
		blobs:        blobs,
		reattributed: map[string][]string{},
		memo:         map[string]string{},
	}
	for p, dir := range bindings {
		h.reattributed[dir] = append(h.reattributed[dir], p)
	}
	global, ok := h.globalHash(rootClosure, bindings, globalPaths, nonGlobal)
	if !ok {
		return nil, false
	}
	h.global = global
	return h, true
}

// NonGlobalRootPaths are the members of the root module's context that are
// deliberately NOT global inputs, and so neither contribute to the global hash
// nor make MemoTrusted refuse a recorded pass: the root dagger.json and the core
// binding dagger regenerates alongside it.
//
// Both change on exactly one kind of PR — adding or removing a module — and
// neither alters what any existing check computes. Leaving them in would retire
// every recorded pass on the change that touches least.
//
//   - dagger.json's engineVersion is a field in every module's dagger.json, and
//     each of the others sits in its own module's source context, so an engine
//     bump already moves every module hash without help from this one. Its
//     toolchains only decide which checks a *workspace-wide* enumeration finds,
//     and this planner enumerates per module instead. Its sdk, source, include
//     and codegen scope the root module itself, which is never memoized — and
//     include and source decide which paths are in the root context at all, so
//     using them to hide a file from the digest removes that file from the global
//     path set and moves the digest anyway.
//   - the core binding is the sharper of the two, because it is the API surface
//     the hasher itself executes, and a hand-patched Glob could make some
//     module's hash go blind to that module's sources — recording a pass on good
//     content and then matching it against bad. What forecloses that is not the
//     digest but Generated: it proves every committed generated file equals what
//     `dagger develop` produces, it belongs to the root module so it always runs,
//     and it is never memoized (and GeneratedSelfTest guards it, after #184). A
//     tampered binding is therefore red at the gate on the very push that would
//     act on it, and reverting to go green restores the honest hash, which the
//     recorded entry no longer matches. So generated files need not be global
//     inputs: a never-memoized check already proves they are derived from inputs
//     that are.
//
// Both subtractions are of a path's *content*, never of set membership: every
// other path in the root closure still contributes, so the authored selection and
// recording machinery stays covered.
//
// Note this is a subtraction from the *global inputs* only. Selection is
// untouched: Attribute still reports a change to either path as global and runs
// everything, because the set of checks really did change.
func NonGlobalRootPaths(rootSource string) []string {
	return []string{"dagger.json", CoreBinding(rootSource)}
}

// GlobalPathsDefault is the default set of path prefixes that govern how CI runs
// rather than what any check computes. They belong to no module's source context,
// so nothing else would attribute them, yet a change to one can invalidate a
// whole run.
func GlobalPathsDefault() []string { return []string{".github/workflows/"} }

// globalHash digests the inputs that belong to no check in particular but can
// invalidate every one of them: the source contexts of the root module's whole
// dependency closure, and everything under globalPaths. It is folded into every
// check's hash, because those paths decide how checks are routed and how a pass
// is recorded — a check's own closure never reaches them.
//
// Two subtractions: the per-toolchain aggregator bindings, which live in the root
// module's context but are attributed to their own toolchain, and nonGlobal.
func (h *Hasher) globalHash(
	rootClosure map[string]bool,
	bindings map[string]string,
	globalPaths []string,
	nonGlobal []string,
) (string, bool) {
	var paths []string
	for _, dir := range sortedKeys(rootClosure) {
		set, resolved := h.srcs[dir]
		if !resolved {
			return "", false
		}
		for p := range set {
			if _, moved := bindings[p]; moved {
				continue
			}
			if slices.Contains(nonGlobal, p) {
				continue
			}
			paths = append(paths, p)
		}
	}
	for p := range h.blobs {
		if hasAnyPrefix(p, globalPaths) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	paths = slices.Compact(paths) // a module may appear in more than one context

	d := newDigest("global")
	for _, p := range paths {
		oid, known := h.blobs[p]
		if !known {
			return "", false
		}
		writeField(d, p)
		writeField(d, oid)
	}
	return sum(d), true
}

// Check returns the input hash a pass on the named check may be recorded under:
// a digest of the check's name, the source context of every module in dirs (its
// owning module's dependency closure) and the global inputs.
//
// ok=false means "unhashable", which callers must treat as "always run": a module
// whose source context could not be read, or a source path with no object id at
// HEAD (untracked, or a dirty working tree, as when running locally).
func (h *Hasher) Check(name string, dirs map[string]bool) (string, bool) {
	d := newDigest("check")
	writeField(d, name)
	writeField(d, h.global)
	for _, dir := range sortedKeys(dirs) {
		mh, ok := h.moduleHash(dir)
		if !ok {
			return "", false
		}
		writeField(d, dir)
		writeField(d, mh)
	}
	return sum(d), true
}

// moduleHash digests one module's source context as sorted (path, object id)
// pairs, memoized by directory because a shared module appears in most closures.
// A memo entry of "" records "unhashable".
//
// A source path with no object id at HEAD makes the module unhashable: it is an
// input this cannot see, and hashing it as absent would record a pass under a
// digest that ignores real content. The reattributed bindings are the exception —
// they are omitted when untracked rather than fatal, because they are generated
// *from* the paths above and a consumer who gitignores generated files would
// otherwise never memoize anything. Omission is deterministic given the tree, and
// a binding that becomes tracked moves the digest rather than colliding with it.
func (h *Hasher) moduleHash(dir string) (string, bool) {
	if cached, seen := h.memo[dir]; seen {
		return cached, cached != ""
	}
	set, resolved := h.srcs[dir]
	if !resolved {
		h.memo[dir] = ""
		return "", false
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	d := newDigest("module")
	writeField(d, dir)
	for _, p := range paths {
		oid, known := h.blobs[p]
		if !known {
			h.memo[dir] = ""
			return "", false
		}
		writeField(d, p)
		writeField(d, oid)
	}
	derived := slices.Clone(h.reattributed[dir])
	sort.Strings(derived)
	for _, p := range derived {
		if oid, known := h.blobs[p]; known {
			writeField(d, p)
			writeField(d, oid)
		}
	}
	digest := sum(d)
	h.memo[dir] = digest
	return digest, true
}

// MemoTrusted reports whether recorded passes may be honoured for this change
// set.
//
// Pass records are written by the same CI run that produced them, so a change
// that can alter the recording machinery could record a pass no check earned.
// Everything that machinery is made of lives under globalPaths or in the root
// module's closure — which is exactly what makes Attribute return global — so
// declining to memoize whenever a global input changed closes that hole by
// construction. It is closed twice over, in fact: such a change also perturbs the
// global hash, so any entry recorded under it lands in a hash space no untampered
// tree ever computes, and reverting the tamper restores the honest hashes rather
// than matching the forged ones.
//
// The predicate is Attribute's own, re-run over the change set with nonGlobal
// subtracted, so there is no second copy of the rule to drift — deletions, the
// aggregator-binding reattribution and the source-context test all behave
// identically here and in selection. Those paths are subtracted for the reasons
// documented on NonGlobalRootPaths, and only from *this* judgement: selection
// still treats them as global and runs everything, because the set of checks
// really did change. The two together are what make adding a module run the new
// module's checks and the root's while every untouched check is retired by its
// unchanged hash.
//
// A change set that is empty — or empty once those paths are removed — means "no
// usable diff" or "nothing that governs recording", not "a global input changed".
// Selection may still fall back to everything there, and memoization is both safe
// and most valuable, because the hashes are read out of HEAD and never depend on
// the diff.
func MemoTrusted(
	changes []Change,
	moduleDirs []string,
	srcs map[string]map[string]bool,
	bindings map[string]string,
	globalPaths []string,
	nonGlobal []string,
) bool {
	rest := make([]Change, 0, len(changes))
	for _, c := range changes {
		if slices.Contains(nonGlobal, c.Path) {
			continue
		}
		rest = append(rest, c)
	}
	if len(rest) == 0 {
		return true
	}
	_, global := Attribute(rest, moduleDirs, srcs, bindings, globalPaths)
	return !global
}

// MemoFilter splits entries into the ones that must still run and the ones a
// previous run already proved good.
//
// An entry with an empty hash — a root-module check, an unreadable source context
// — is never skipped, per the rule from #170: never skip a check a change could
// plausibly affect.
func MemoFilter(entries []Entry, knownGood map[string]bool) (run, skipped []Entry) {
	for _, e := range entries {
		if e.Hash != "" && knownGood[e.Hash] {
			skipped = append(skipped, e)
			continue
		}
		run = append(run, e)
	}
	return run, skipped
}

func newDigest(kind string) hash.Hash {
	h := sha256.New()
	writeField(h, hashVersion)
	writeField(h, kind)
	return h
}

// writeField feeds s to h length-prefixed, so that no combination of paths and
// object ids can produce the same byte stream as a different combination. Git
// paths may contain any byte except NUL, newlines included, so a separator
// character would not be enough.
func writeField(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	h.Write(n[:])
	io.WriteString(h, s) //nolint:errcheck // hash.Hash never fails
}

func sum(h hash.Hash) string {
	return hex.EncodeToString(h.Sum(nil))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
