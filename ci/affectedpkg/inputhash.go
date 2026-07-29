package affectedpkg

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
const hashVersion = "z5labs/devex ci input-hash v1"

// InputHashes returns, per check name, a digest of everything that determines
// what that check computes: the check's own name, the source context of every
// module in its dependency closure, and the global inputs that govern how CI
// runs at all. Two runs agree on a check's hash exactly when nothing the check
// can read has changed between them, which is what makes a recorded pass
// reusable (#238).
//
// The digest is built from **git blob object ids**, never Dagger digests.
// Dagger's Directory.digest is explicitly not stable across engine releases, so
// a persisted set keyed on it would be invalidated wholesale by every engine
// bump; git object ids are content hashes git itself already maintains. An
// engine bump still perturbs every hash, correctly — engineVersion is a field in
// every module's dagger.json, and each dagger.json is in its own module's source
// context.
//
//   - closure is the per-check dependency closure from BuildClosures. A check
//     absent from it is unresolved and gets no hash.
//   - srcs maps a module directory to the repo-relative paths in its Dagger
//     source context, i.e. precisely what Dagger uploads for it (the same
//     source-context notion Attribute narrows with). It must include RootPkg.
//   - blobs maps a repo-relative path to its git blob object id at HEAD, from
//     gitdiff.HeadBlobs.
//   - bindings is the aggregator-binding reattribution map from
//     AggregatorBindings, applied here for the same reason Attribute applies it
//     (#179): ci/internal/dagger/<toolchain>.gen.go is provably owned by one
//     toolchain, and repo convention requires regenerating it whenever that
//     toolchain's sources shift line numbers. Folding it into the global inputs
//     instead would make the routine binding refresh that accompanies almost
//     every module change perturb every check's hash, and memoization would
//     essentially never hit.
//
// A check is omitted from the result — meaning "unhashable", which callers must
// treat as "always run" — whenever any part of that answer is unavailable: an
// unresolved closure, a module whose source context could not be read, or a
// source path with no object id at HEAD (untracked, or a dirty working tree, as
// when running locally). A nil result means even the global inputs were
// unhashable, so nothing may be memoized.
func InputHashes(
	closure map[string]map[string]bool,
	srcs map[string]map[string]bool,
	blobs map[string]string,
	bindings map[string]string,
) map[string]string {
	reattributed := map[string][]string{}
	for path, dir := range bindings {
		reattributed[dir] = append(reattributed[dir], path)
	}

	global, ok := globalHash(srcs, blobs, bindings)
	if !ok {
		return nil
	}

	memo := map[string]string{}
	out := make(map[string]string, len(closure))
	for name, dirs := range closure {
		// ci:* checks are never memoized, so they need no hash. ci:generated
		// runs codegen for every module in the workspace, and ci's own checks
		// read state its declared closure (the root module alone) does not
		// describe, so a closure-derived hash would not cover their inputs.
		// Select already runs them unconditionally; this keeps that true.
		if isCiCheck(name) {
			continue
		}
		h := newDigest("check")
		writeField(h, name)
		writeField(h, global)
		hashable := true
		for _, dir := range sortedKeys(dirs) {
			mh, resolved := moduleHash(dir, srcs, blobs, reattributed, memo)
			if !resolved {
				hashable = false
				break
			}
			writeField(h, dir)
			writeField(h, mh)
		}
		if !hashable {
			continue
		}
		out[name] = sum(h)
	}
	return out
}

// rootConfig is the repository-root dagger.json, the one member of the root
// module's source context that is deliberately NOT a global input.
//
// Everything it can affect is already accounted for downstream, so folding it in
// would invalidate every recorded pass for a change that alters what no existing
// check computes — most visibly when a new daggerverse module is added, since a
// toolchain entry here is unavoidable:
//
//   - engineVersion is a field in all 47 dagger.json files, and each of the other
//     46 sits in its own module's source context, so an engine bump already moves
//     every module hash without help from this one.
//   - toolchains only decides which checks exist and which module each is
//     enumerated from. Repointing an entry's source changes Check.OriginalModule,
//     hence the closure, hence the hash — the dependency graph carries that on its
//     own. And no non-ci leg ever loads the root module: ci.yml routes each one at
//     its own tests module.
//   - sdk, source, include and codegen scope the root module itself, which is
//     never memoized. Note that include and source decide which paths are in the
//     root context at all, so using them to hide a file from the digest removes
//     that file from globalHash's path set and moves the digest anyway.
const rootConfig = "dagger.json"

// nonGlobalRootPaths are the members of the root module's source context that
// are deliberately NOT global inputs, and so neither contribute to globalHash
// nor make MemoTrusted refuse a recorded pass.
//
// Both change on exactly one kind of PR — adding or removing a daggerverse
// module — and neither alters what any existing check computes. Leaving them in
// would retire every recorded pass on the change that touches least: adding a
// toolchain entry to rootConfig also makes dagger regenerate coreBinding, which
// gains nothing but an ID type and a loader for the new toolchain's object.
//
// coreBinding is the sharper of the two, because it is the API surface the
// hasher itself executes, and a hand-patched Glob could make some module's hash
// go blind to that module's sources — recording a pass on good content and then
// matching it against bad. What forecloses that is not the digest but
// ci:generated: it proves every committed generated file equals what
// `dagger develop` produces, it always runs, and it is never memoized (and
// ci:generated-self-test guards it, after #184). A tampered binding is therefore
// red at the gate on the very push that would act on it, and reverting to go
// green restores the honest hash, which the recorded entry no longer matches. So
// generated files need not be global inputs: a never-memoized check already
// proves they are derived from inputs that are. That is the same reasoning
// AggregatorBindings applies to the per-toolchain bindings (#179), extended to
// the one binding that is attributable to no toolchain at all.
//
// Both subtractions are of a path's *content*, never of set membership: every
// other root-context path still contributes to globalHash, so the authored
// recording machinery under ci/** stays covered.
//
// Note this is a subtraction from the *global* inputs only. Selection is
// untouched: Attribute still reports a change to either path as global and runs
// the full universe, because the set of checks really did change.
var nonGlobalRootPaths = []string{rootConfig, coreBinding}

// globalHash digests the inputs that belong to no check in particular but can
// invalidate every one of them: the root module's source context (ci/**) and
// everything under globalPrefixes. It is folded into every check's hash, because
// those paths decide how checks are routed and how a pass is recorded — the
// closures alone never reach them.
//
// Two subtractions: the per-toolchain aggregator bindings, which live in the root
// module's context but are attributed to their own toolchain (see InputHashes),
// and nonGlobalRootPaths.
func globalHash(srcs map[string]map[string]bool, blobs map[string]string, bindings map[string]string) (string, bool) {
	root, resolved := srcs[RootPkg]
	if !resolved {
		return "", false
	}
	paths := make([]string, 0, len(root))
	for p := range root {
		if _, moved := bindings[p]; moved {
			continue
		}
		if slices.Contains(nonGlobalRootPaths, p) {
			continue
		}
		paths = append(paths, p)
	}
	for p := range blobs {
		if hasAnyPrefix(p, globalPrefixes) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	h := newDigest("global")
	for _, p := range paths {
		oid, known := blobs[p]
		if !known {
			return "", false
		}
		writeField(h, p)
		writeField(h, oid)
	}
	return sum(h), true
}

// moduleHash digests one module's source context as sorted (path, object id)
// pairs, memoized by directory because a shared module (random, crypto, ...)
// appears in most closures. A memo entry of "" records "unhashable".
func moduleHash(
	dir string,
	srcs map[string]map[string]bool,
	blobs map[string]string,
	reattributed map[string][]string,
	memo map[string]string,
) (string, bool) {
	if cached, seen := memo[dir]; seen {
		return cached, cached != ""
	}
	set, resolved := srcs[dir]
	if !resolved {
		memo[dir] = ""
		return "", false
	}
	paths := make([]string, 0, len(set)+len(reattributed[dir]))
	for p := range set {
		paths = append(paths, p)
	}
	paths = append(paths, reattributed[dir]...)
	sort.Strings(paths)

	h := newDigest("module")
	writeField(h, dir)
	for _, p := range paths {
		oid, known := blobs[p]
		if !known {
			memo[dir] = ""
			return "", false
		}
		writeField(h, p)
		writeField(h, oid)
	}
	digest := sum(h)
	memo[dir] = digest
	return digest, true
}

// MemoTrusted reports whether recorded passes may be honoured for this change
// set.
//
// Pass records are written by the same CI run that produced them, so a change
// that can alter the recording machinery could record a pass no check earned.
// Everything that machinery is made of lives under globalPrefixes or in the root
// module's source context — which is exactly what makes Attribute return global —
// so declining to memoize whenever a global input changed closes that hole by
// construction. It is closed twice over, in fact: such a change also perturbs
// globalHash, so any entry recorded under it lands in a hash space no untampered
// tree ever computes, and reverting the tamper restores the honest hashes rather
// than matching the forged ones.
//
// The predicate is Attribute's own, re-run over the change set with
// nonGlobalRootPaths subtracted, so there is no second copy of the rule to drift
// — deletions, the aggregator-binding reattribution and the source-context test
// all behave identically here and in selection. Those paths are subtracted for
// the reasons documented on them, and only from *this* judgement: selection still
// treats them as global and runs the full universe, because the set of checks
// really did change. The two together are what make adding a daggerverse module
// run the new module's checks and ci:* while every untouched check is retired by
// its unchanged hash.
//
// A change set that is empty — or empty once those paths are removed — means "no
// usable diff" or "nothing that governs recording", not "a global input changed".
// Selection may still fall back to the full universe there, and memoization is
// both safe and most valuable, because the hashes are read out of HEAD and never
// depend on the diff.
func MemoTrusted(changes []Change, moduleDirs []string, srcs map[string]map[string]bool, bindings map[string]string) bool {
	rest := make([]Change, 0, len(changes))
	for _, c := range changes {
		if slices.Contains(nonGlobalRootPaths, c.Path) {
			continue
		}
		rest = append(rest, c)
	}
	if len(rest) == 0 {
		return true
	}
	_, global := Attribute(rest, moduleDirs, srcs, bindings)
	return !global
}

// MemoFilter splits kept into the checks that must still run and the checks a
// previous run already proved good.
//
// A check with no entry in hashes — a ci:* check, an unresolved closure, an
// unreadable source context — is never skipped, per the rule from #170: never
// skip a check a change could plausibly affect.
func MemoFilter(kept []string, hashes map[string]string, knownGood map[string]bool) (run, skipped []string) {
	for _, name := range kept {
		if h, ok := hashes[name]; ok && knownGood[h] {
			skipped = append(skipped, name)
			continue
		}
		run = append(run, name)
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
	io.WriteString(h, s)
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
