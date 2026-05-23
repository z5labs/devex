# Dagger engine internals

[docs.dagger.io](https://docs.dagger.io/) documents Dagger's public API
well, but it has no pages explaining the engine's *under-the-hood*
behavior. As this repo's `daggerverse/` modules have grown —
cross-module type passing, service networking, container-heavy CI test
suites — we keep running into questions whose answers are not written
down anywhere official, and re-deriving them the hard way.

These pages capture that research as reviewable, in-repo reference
documentation. They are written for a contributor who already knows how
to *use* Dagger but needs to reason about how it actually works.

## Engine version

All research here is pinned to the Dagger engine version this repo
currently uses — **`v0.20.8`** (see the `engineVersion` field in the
root `dagger.json` and every per-module `dagger.json`).

Source citations point at the `dagger/dagger` repository at the commit
the `v0.20.8` tag resolves to:
[`74bff7d10fd78dd6935c60c4514558598f216451`](https://github.com/dagger/dagger/tree/74bff7d10fd78dd6935c60c4514558598f216451).
Engine internals drift between releases — treat these pages as accurate
for `v0.20.8` and re-verify against source before assuming they hold on
a newer engine.

## How to read these docs — the sourcing rule

Every non-trivial claim in these pages is **either**:

- **(a) source-backed** — cited with a permalink to a specific
  file/line in `dagger/dagger`, pinned to commit `74bff7d…` (a tag or
  commit, never `main`); **or**
- **(b) experiment-backed** — substantiated by a reproducible
  experiment, with the exact `dagger` command **and** its observed
  output included inline.

Experiments are self-contained and re-runnable by a reader: they rely
on no local-only state, and where an experiment needs a scratch
module or container the page includes the minimal setup steps.

Each page ends with an **"Open questions / unverified"** section listing
anything that could not be confirmed by source or experiment — gaps are
made explicit rather than glossed over.

## Pages

- **[module-importing.md](./module-importing.md)** — how a module
  declares, loads, and consumes another module; why types can be shared
  across SDKs through the GraphQL boundary but native language types
  cannot cross a module boundary.
- **[networking.md](./networking.md)** — the service/networking model;
  whether services and ports are scoped or global (per session); and
  whether containers can be placed into isolated networks (they cannot
  — there is one flat network, scoped by DNS).
- **[container-execution.md](./container-execution.md)** — what a
  container exec does inside the engine, how it maps onto BuildKit LLB,
  what it costs in CPU/memory/disk, and what all of that means for this
  repo's CI.
- **[caching-and-evaluation.md](./caching-and-evaluation.md)** — when a
  pipeline actually runs (terminal vs. non-terminal selections, parallel
  branch resolution) and the two caches that decide whether work
  repeats: the dagql per-session call cache and the BuildKit
  content-addressed operation cache, plus cache mounts and the prune/GC
  story for disk reclamation.
