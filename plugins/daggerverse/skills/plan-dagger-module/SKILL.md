---
name: plan-dagger-module
description: Standardized 9-step workflow for designing a new daggerverse module from scratch — picking a name, researching prior art, proposing types/funcs and tests, drafting story issues against `.github/ISSUE_TEMPLATE/story.yaml`, and creating them with `gh issue create`. Use this whenever the user wants to start a new daggerverse module, kicks off a new module design under `daggerverse/`, or asks to "scope out", "plan", "design", or "propose issues for" a new module (Redis, Vault, MongoDB, NATS, anything). Use it even when the user types `/plan-dagger-module` without naming a module — drive the workflow and ask. Skip only when the user is *implementing* an already-scoped module (issue already exists) or making changes to an existing module's API.
---

# plan-dagger-module

A new daggerverse module starts as a design conversation, not a code patch. This skill paces that conversation: name → research → API → tests → issues → `gh issue create`. The point is to *converge with the user* before any module code is written, so the eventual implementation PR has a real spec to land against.

You are not the decider here. The user is. Your job is to surface options, encode constraints, and stop for input — not to charge through to a final answer.

## Operating principles

- **Nine steps, in order.** No reordering, no fusing two steps into one, no jumping ahead because a step felt obvious. Each step has its own output and its own checkpoint.
- **Stop after every step.** Output what the step produced, ask for feedback, and *wait*. Do not start the next step in the same turn. The "refine" steps (4, 6, 8) loop on the previous propose step until the user explicitly approves.
- **Echo the constraints back into proposals.** Every proposed type, function, and test must be consistent with the rules in *Constraints* below. If a constraint forces a design choice (e.g. a function returning a `*dagger.Secret` instead of a string), say so in the proposal rather than burying it.
- **Use existing modules as shape references.** Read at least one comparable module under `daggerverse/` before proposing types. Cite the analogy in your proposal (e.g. "modeled after `kafka.Cluster` — controller + broker services with `+cache=\"never\"` chained methods").
- **Don't invent caching policy.** The default `+cache=` story is: pure deterministic helpers get no directive (7-day default is fine); anything that spins up services, exposes randomness, or chains queries needs `+cache="never"` on every method that gets chained off it. Tests that exist to *prove* non-caching use `+cache="session"`.
- **Every identified work item becomes an issue — or is explicitly dropped.** Any API chunk, extension, or capability surfaced during design (steps 2–7) is *work*. The flow is **not complete** until each such item is either captured in a created GitHub issue or explicitly dropped by the user. Nothing survives as prose-only — a sentence like "TLS support can come in a follow-up" is not a plan, it's a leak. The chained `Ci` builder is the canonical item that gets mentioned and then lost (it had to be retrofitted onto zig and others as a later story instead of falling out of the original design pass); treat it as a first-class follow-up issue every time it comes up. The step-9 completeness gate enforces this.

## The nine steps

### Step 1 — Pick a name

Ask the user what the module is for, then propose **two or three** lowercase, kebab-case names with one-line rationales. Names should:

- Match the upstream project name where one exists (`redis`, `vault`, `nats`), or describe the capability (`certificate-management`, `grafana-stack`).
- Avoid generic terms (`db`, `infra`, `tools`) — daggerverse modules are scoped to a single product or capability.
- Avoid colliding with directories already under `daggerverse/`. List existing module names so the user can see the field.

**Stop. Ask the user to pick one (or propose their own).** Do not proceed without an explicit name.

### Step 2 — Research related things

With the chosen name in hand, do the research **before** writing any API. Search in parallel:

- `daggerverse/` for the closest existing module — read its `main.go`, `dagger.json`, and `tests/main.go`. Note the topology (single struct vs. nested objects), how it handles services, and what it returned to callers.
- Open PRs and recent story issues with `gh issue list --search "story" --state all --limit 20` and `gh pr list --state merged --limit 10`. Skim the bodies of the two or three most relevant ones for the shape (#11, #16, #17, #23 are good anchors — they show how the team writes proposals).
- Upstream docs / Docker images for the thing you're wrapping (canonical image name, default ports, security profile shape).
- Read `daggerverse/CLAUDE.md` once (function caching, regeneration rules, module layout, name mangling).

Output a short research summary: closest analog module, upstream image + version, defaults that matter, anything surprising. Keep it tight — half a page, not a dissertation.

**Stop. Ask "anything I'm missing before I propose the API?"** Wait for the user's response.

### Step 3 — Propose types + funcs

Draft the module's public surface as a code-shaped sketch (Go signatures, no bodies). Cover:

- The root receiver type (`type Redis struct{}`).
- Auxiliary types the module exposes — services, clients, security profiles, anything with its own methods. Mark private fields with `// +private`.
- Each function: name, parameters with `+default=` / `+optional=` directives where relevant, return type, and a one-line doc comment explaining what it does.
- The `+cache=` directive on every function and every method that gets chained off a returned object (per the chained-method rule — see *Constraints*).
- Where `*dagger.File` / `*dagger.Secret` are used at module boundaries instead of strings, and why.
- A `tests/` subpackage with a `Tests` struct, `All(ctx)` aggregator with `// +check`, and the planned test method names (bodies come in step 5).

Cite the analog module(s) you modeled this after and call out anywhere the design diverges from the analog and why.

**Stop. Ask for feedback on the API shape.** Do not enumerate tests yet — that is step 5.

### Step 4 — Refine types + funcs

Apply the user's feedback. If they ask for a change, restate the affected part of the API with the change applied, and re-ask. **Loop here** — keep refining until the user explicitly says some variant of "looks good, move on", "approved", or "ship it for tests now". Do not advance to step 5 on ambiguous responses; ask.

### Step 5 — Propose minimum required tests

Now design the `tests/` package. Output a list of test methods on the `Tests` struct, each with:

- A method name — **exported PascalCase/UpperCamelCase** in Go (e.g. `UuidV4ShouldNotBeCached`). Methods must be exported or Dagger will not expose them as functions. The CLI name is derived by name mangling (`UuidV4ShouldNotBeCached` → `uuid-v-4-should-not-be-cached`); don't try to control the CLI name by lower-casing the Go method.
- One-line description of what behavior it pins down.
- Which prod function(s) it exercises and what failure mode it catches.

Required coverage to consider — propose tests for whichever apply to this module:

- **Cache correctness** — anything with `+cache="never"` needs a `XShouldNotBeCached` test that calls it twice and asserts different results (for random) or that side effects ran (for services). `random/tests/main.go` is the template.
- **Round-trip / smoke** — for clients (Kafka.Client, future Redis.Client), one create/read/delete round-trip per resource type.
- **Service topology** — for modules that spin up containers, a test that binds the service into a fresh container and reaches it.
- **Defaults** — one test per defaulted parameter that asserts the default produces a working result.
- **Rejected inputs** — explicit tests for parameter combinations the module refuses (e.g. kafka's `controllers > 1` rejection).
- **File / secret outputs** — round-trip via `dag.CurrentModule().WorkdirFile` so the file is materialized in the engine and re-readable.

Test IDs and names should not bake in caller-supplied secrets, passwords, or other constants — generate them with `dag.Random().Sha256(ctx)` at test time (see *Constraints / no hardcoded secrets*).

**Stop. Ask for feedback on the test list.**

### Step 6 — Refine tests

Same loop as step 4. Apply feedback, restate the affected entries, re-ask. Wait for explicit approval before moving on.

### Step 7 — Propose GitHub issues

Render the work as one or more story issues. Most new modules ship as **one main issue** covering the whole API and a **separate follow-up issue for each clearly separable extension** (TLS support, additional security profiles, optional integrations, the chained `Ci` builder). When in doubt about whether a chunk belongs in the main issue or its own, that's a question for the user — but it must land in *some* issue. Issue #11 / #16 (kafka main) and #17 (kafka isolated-defaults follow-up) are good shape references.

**Every separable extension gets its own fully drafted issue — title + complete body — not a sentence buried in the main issue's Description.** The recurring failure this skill exists to prevent: flagging something like "a chained `Ci` builder would be a natural follow-up" in prose, then never creating an issue for it. The `Ci` builder is the canonical offender; it has been deferred-and-forgotten across multiple module designs and only got tracked when retrofitted after the fact. If you mention an extension as future work *anywhere* in steps 2–7, you owe it a drafted issue here (or an explicit drop from the user at the step-9 gate).

**Follow-up title format:** `story(issue-<main#>): <short description>`, where `<main#>` is the main issue's number. That number isn't known until the main issue is actually created — so draft follow-up titles with an `issue-<main#>` placeholder now, and step 9 fills in the real number after creating the main issue. Each follow-up's `### Related Issues` section references the main issue.

**Title format — exactly:** `story(<subject>): <short description>`

- `<subject>` is usually `daggerverse` for a brand-new module, or `issue-<N>` if this story closes a specific prior issue. Match the convention of existing issues (#11 uses `daggerverse`, #16 uses `issue-11`, #17 uses `daggerverse`).
- `<short description>` is lowercase, no trailing period, under ~70 chars total title length.

**Body — match `.github/ISSUE_TEMPLATE/story.yaml` exactly:**

```markdown
### Description

<End-user-perspective paragraph: what the module does and who would use
it. Then a "#### <subsection>" for each logical chunk of the API —
factories, services, clients, security profiles — with the Go signatures
in a single fenced block per subsection. Call out defaults and any
explicit rejections. End with the security/TLS posture for this story
("plaintext only; TLS in a follow-up") if applicable.>

### Acceptance Criteria

- [ ] <Each criterion is a concrete, testable outcome>
- [ ] <Functions listed in Description exist and pass `dagger functions`>
- [ ] <Each test from step 6 exists and passes individually>
- [ ] <Specific behaviors: defaults work, rejected inputs are rejected,
      cache directives propagate on chained methods, etc.>

### Related Issues

<Reference any prior issue numbers this story builds on or closes, or
`_None._` if standalone.>
```

Output the full proposed title and body for each issue. **Stop. Ask for feedback on each issue's title, body, and split.**

### Step 8 — Refine issues

Same loop as steps 4 and 6. Apply feedback to title/body/split, restate, re-ask. Wait for explicit approval — **the main issue and every follow-up must each be individually approved** before step 9; blanket "looks good" on the bundle is fine only if you restate the full list (main + each follow-up) so the user is approving every item knowingly. If the user wants to drop one of the proposed issues, drop it (record it as an explicit drop for the step-9 gate); if they want to add one, add it and run it through step 7's format.

### Step 9 — Gate completeness, then create issues on GitHub

**First, the completeness gate. Do this before any `gh issue create`.** Restate the full list of *every* work item identified across steps 2–7 — the main API surface, each separable extension, the chained `Ci` builder, and anything you flagged as future/follow-up work in prose anywhere in the design conversation. For each item, force a binary outcome:

- **an approved issue exists for it** (main or a step-8-approved follow-up), or
- **the user explicitly drops it** ("skip the `Ci` builder for now").

Do not proceed while any item is unresolved, and do not let an item slip by unmentioned. If you're unsure whether something counts as work, list it and ask — the cost of an extra line in the gate is trivial; the cost of a silently-lost follow-up is a retrofit story months later. This gate is the whole point of the skill: **nothing surfaced during design disappears without a decision.**

Once every item is either an approved issue or an explicit drop, create the issues. **Create the main issue first**, capture its number from the returned URL, then create each approved follow-up — substituting the real main issue number into its `story(issue-<main#>): ...` title and its `### Related Issues` reference.

```bash
gh issue create --title "story(<subject>): <description>" --body "$(cat <<'EOF'
### Description

...

### Acceptance Criteria

- [ ] ...

### Related Issues

...
EOF
)"
```

Use a HEREDOC so markdown formatting (lists, code fences) survives shell quoting. Do not pass `--assignee`, `--label`, or `--milestone` unless the user asked for them. Print the returned issue URL after each `gh issue create` succeeds.

If `gh issue create` fails, surface the error to the user and ask how to proceed — do **not** retry blindly or fall back to `curl` against the GitHub API.

**The flow is complete only when every gated item is either a printed issue URL or an explicit drop.** Declaring the design done while a flagged extension still lives only in prose is the exact failure mode this skill exists to prevent — do not do it.

## Constraints

These are non-negotiable. Every proposal in steps 3, 5, 7 must respect them; if a user request would violate one, surface the conflict explicitly rather than silently compromising.

- **No hardcoded secrets, even in tests.** Test passwords, cluster IDs, topic names, and other identifiers are generated at runtime via `dag.Random().Sha256(ctx)` (preferred when the dep is already in scope) or `crypto/rand` in Go module code. Literal credential strings never enter git, not even as placeholders.
- **`+cache="never"` must repeat on every chained method.** If a function returns an object and callers will chain methods off that object, every method on that returned type also needs `+cache="never"` — otherwise repeated chained queries serve stale cached values. The kafka `Cluster` and `Client` are the canonical example.
- **No external module types across module boundaries.** A Dagger function cannot accept or return another module's exported types. Pass `*dagger.File` and `*dagger.Secret` across boundaries and re-load via the dep's `Load*` helpers inside the receiving module.
- **Runtime I/O is pure Go.** Do not spin up an alpine helper container just to move bytes. Two distinct paths:
  - **Reading an input `*dagger.File`:** `file.Export(ctx, localPath)` materializes it onto the module's local filesystem, then read it with `os.Open` / `os.ReadFile`. See `daggerverse/crypto/main.go:digestFile` and `daggerverse/certificate-management/main.go` for the pattern.
  - **Returning an output `*dagger.File`:** write the bytes under a subdir of `dag.CurrentModule().Workdir` with `os.WriteFile` (or equivalent), then return `dag.CurrentModule().WorkdirFile(relPath)`. See the `writeWorkdirFile` helper in `daggerverse/crypto/main.go` and `daggerverse/grafana-stack/main.go`. Do **not** use `Export` for outputs — that is the input direction.
- **Render YAML with `gopkg.in/yaml.v3`.** Any function that produces YAML (config files, compose-style fragments) uses `yaml.v3`'s `Marshal` or `Encoder` — never `fmt.Fprintf` or string concatenation. Hand-rolled YAML mishandles quoting and escaping of caller-supplied strings.
- **`+cache=` directives go on their own line in the doc comment block above the function.** Place above the signature; do not inline.
- **Function name mangling is real.** Go method `Sha256ShouldNotBeCached` becomes `sha-256-should-not-be-cached` on the CLI; `UuidV4` becomes `UUIDV4(ctx)` on the dag client (acronyms uppercase in generated bindings). Account for this when discussing CLI invocation in the issue body.
- **`dagger develop` regenerates bindings.** After signature changes, both the module *and* any module that depends on it (`tests/` depends on `..`) need `dagger develop` re-run. Mention this in acceptance criteria when the module has a `tests/` subpackage.

When asked about external traces or runs, use `dagger trace <id> --progress=plain` — never `curl` against `dagger.cloud`.

The deeper reference for cache rules, regeneration, layout, and name mangling is `daggerverse/CLAUDE.md`. Read it once during step 2 research.

## Shape references

Read whichever applies during step 2 research.

- `daggerverse/random/main.go` — simplest module: pure helpers, `+cache="never"` on each, no services. Good template for any module that's "just functions".
- `daggerverse/crypto/main.go` — helpers that consume secrets and emit files; good template for `*dagger.Secret` + `*dagger.File` at boundaries.
- `daggerverse/certificate-management/main.go` — multiple cooperating types (CA + cert + key), `Load*`-style entry points.
- `daggerverse/grafana-stack/main.go` — multi-service module with provisioned configs rendered via yaml.v3.
- `daggerverse/kafka/main.go` — services + pure-Go client, `+cache="never"` propagation on chained methods, runtime file output via `dag.CurrentModule().WorkdirFile`, explicit input rejection.
- Issues #11, #16 (kafka main module + delivery PR) and #17 (focused follow-up) — canonical story-issue bodies. View with `gh issue view <N>` during step 7.

## Stopping correctly

A common failure mode is "I'll do steps 3 and 4 together because the answer felt obvious." Don't. The user explicitly invokes this skill *because* they want a paced design conversation; collapsing steps defeats the purpose and erodes their ability to course-correct cheaply. If a step's output feels trivially small, that's fine — output it, stop, and let the user tell you to move on.

When you stop, end the turn with a single clear question like:

> Step 3 done — API shape above. Move on to test design, or refine first?

Not five questions, not a paragraph of caveats. One question. Wait.
