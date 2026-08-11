---
name: setup-backlog
description: Bootstrap a repository for the unattended backlog cycle and verify the environment it depends on — write `.claude/backlog.json` with the label, dependency convention and verify commands inferred from the repository, install the auto-merge workflow, create the backlog and merge labels, and check squash-only merging, the default branch's required status checks and whether Copilot code review is actually enabled. Use this whenever the user wants to set up, bootstrap, install, onboard or enable the backlog loop on a repository, when `backlog:next-issue` stops because `.claude/backlog.json` is missing, or when the user wants to know why an unattended run keeps stalling. Skip this when the repository is already configured and the user simply wants work done — that is `backlog:next-issue` or `backlog:run-backlog`.
allowed-tools: Bash, Read, Write, Edit, Glob, Grep, AskUserQuestion
---

# setup-backlog

Get a repository ready to run its backlog unattended, and — the half that actually saves
time — check the things that will otherwise fail three iterations into an unattended run
before anyone notices.

Copying a workflow file in is easy. The failures that cost a run are environmental: Copilot
code review not enabled for the organisation, a merge method other than squash still
allowed, a default branch with no required status check so the merge label lands the pull
request instantly and unverified. Those are the point of steps 4 and 5, and several of them
you can only *report*, because fixing them takes repository-admin rights this agent does not
have.

Work through the steps in order and finish with one report covering all of them.

## 0. Preflight

```
git rev-parse --show-toplevel
gh auth status
read -r REPO DEFAULT_BRANCH <<<"$(gh repo view --json nameWithOwner,defaultBranchRef \
  --jq '"\(.nameWithOwner) \(.defaultBranchRef.name)"')"
```

Everything below runs from the repository root. `<repo>` and `<default-branch>` are those
two values — resolved from GitHub, never written into any file this skill produces.

Check the token's scopes in the `gh auth status` output while you are here. Pushing a file
under `.github/workflows/` requires the `workflow` scope; without it step 3's file writes
fine and the push that carries it is rejected, which is a confusing place to discover it.

## 1. Infer the configuration

Do not interview the user for what the repository already says.

### `verify`

**CI wins.** Read `.github/workflows/*.y*ml` and take the commands the pull request workflow
actually runs. The entire value of this list is that a local pass predicts a green pull
request, so a command CI runs that is missing here becomes a red pull request the cycle then
iterates on, and a command here that CI does not run is wasted time on every iteration.

Where CI is unreadable or absent, fall back on what the repository is built from. Union the
rows that match, and dedupe:

| detector | fallback commands |
| --- | --- |
| `go.mod` | `go build ./...`, `go vet ./...`, `go test -race ./...`, `gofmt -l .` |
| `dagger.json` | `dagger functions` — it loads and type-checks the module, which is the cheap smoke test; prefer whatever `dagger call` CI runs |
| `package.json` | the subset of `lint`, `test`, `build` that actually exists in `.scripts`, as `npm run <name>` (`npm test` for `test`) |

Two extras worth catching from CI rather than from the fallback table, because they are
common and neither is implied by `go.mod`: a linter run as its own job (`staticcheck ./...`,
`golangci-lint run`), and a tool with a version floor. If CI pins a tool version, say so in
the report — a stale local binary failing on a version mismatch is not a code defect, and an
unattended run will otherwise spend its three fix attempts on it.

### `dependencies.style`

Read an actual issue body. Do not ask.

```
gh issue list --state open --limit 20 --json number --jq '.[].number'
```

Fetch a handful of those bodies (`gh issue view <n> --json body --jq .body`) and look for
which convention is in use:

- A line opening with `Blocked by:` followed by `- #N` items → `blocked-by`.
- `Depends on #N` written inline → `depends-on`.
- Neither, across every body you read → `none`.

Confirm the guess against the extractor the cycle will actually use, rather than against
your reading of the body — they are not the same thing, and the one that decides eligibility
is the extractor:

```
gh issue view <n> --json body --jq .body \
  | "${CLAUDE_PLUGIN_ROOT}/scripts/select-issue.sh" --extract <style>
```

It prints one reference per line — `#14` for this repository, `owner/repo#N` for anywhere
else — and nothing when the convention is absent. A style that extracts nothing from bodies
that plainly declare ordering means the repository uses a third convention neither value
models; say so rather than picking the closer of the two.

Say in the report which issues you read and what you concluded. `none` inferred from a
backlog that has simply not declared any ordering *yet* is the one inference worth
double-checking with the user, because it is indistinguishable from a convention you did not
sample — and getting it wrong means work happens in the wrong order rather than not at all.

**Never infer `native`.** The fourth style reads GitHub's typed issue dependencies instead of
the body, and it is the better one where a repository has populated them — but an
unpopulated edge set and an unblocked issue are the same response, so a repository that has
not adopted them would come back "nothing blocks this" for every issue. Only the operator can
say which of those two a quiet repository is, so `native` is written into the config
deliberately or not at all.

What you *can* do is tell the user it is available, with evidence. This counts the typed
edges across the backlog you already listed:

```
for n in <the numbers you listed>; do
  printf '%s %s\n' "$n" "$(gh api "repos/<repo>/issues/$n/dependencies/blocked_by" --jq length)"
done
```

The REST route is enough to count; `select-issue.sh` reads the same edges over GraphQL
because it needs the owning repository named on each one, which this shape does not carry.

A repository with edges already populated on issues whose bodies *also* declare ordering in
prose is the clear case for `native`, and worth raising. A repository with none anywhere is
not evidence against it — it is evidence that nobody has written any yet. Report the counts
either way and let the user choose; do not switch the style on their behalf.

### The rest

- `select.label` — the label the backlog uses. `gh label list --json name --jq '.[].name'`;
  if `story` exists, take it. If the repository clearly uses another (`feature`, `task`),
  take that and say so. If none exists, `story`, and step 4 creates it. A run that wants
  another for once passes `select-issue.sh --label <name>`, so this is a default and not a
  commitment.
- `select.milestone` — **`null`, unless the user asks for one.** Not inferred.

  It used to be: "exactly one open milestone with open issues is a reasonable inference".
  It is a reasonable inference *about the moment you ran it*, and that is the problem — the
  value is a snapshot committed to a tracked file, and it decays silently as releases ship.
  Measured: a repository bootstrapped mid-release-train got `select.milestone: "v0.2.0"`, the
  milestone was later deleted, and every subsequent run reported `BACKLOG EMPTY` over an
  open, eligible, unmilestoned story. `select-issue.sh` now refuses an unknown milestone
  loudly rather than selecting from nothing, so the trap no longer swallows a run — but the
  best value for a key that decays is the one that cannot.

  If the user does want a milestone pinned, `gh api repos/<repo>/milestones --jq '.[] |
  "\(.title) \(.open_issues)"'` lists the candidates. Write it, and say in the report that it
  will need changing when that milestone closes, and that `select-issue.sh --milestone
  <title>` and `--no-milestone-filter` override it per run so no edit is needed to work
  another one.
- `select.limit` — `200`.
- `select.project` — **omit it unless the user asks.** It scopes selection to one value of a
  single-select field on a GitHub project (`Module`, `Area`, `Component`, `Status`), which is
  worth having only where the repository actually groups its work that way; absent, selection
  makes no project API call at all. When it is wanted, `gh project list --owner <owner>` gives
  the number and `gh project field-list <number> --owner <owner>` gives the field names — both
  need the `read:project` token scope, which `repo` does not include, so say so in the report
  along with `gh auth refresh -s read:project`.

  Write `owner` and `number` — which board the work lives on is a property of the repository,
  and they are the only keys the block requires. Write `field` as the axis the backlog is
  *usually* grouped by, and leave `value` as `null`. Both of those are per-run choices that
  `select-issue.sh --project-field <name>` and `--project-value <value>` override, because
  neither "just this module today" nor "the In Progress ones today" is a permanent
  description of the backlog. Every key here has such a flag, `--project-owner` and
  `--project-number` included, so writing this block is a convenience and never a
  prerequisite for a scoped run.
- `merge.label` / `merge.workflow` — `auto-merge` and `auto-merge.yaml`, matching the asset
  step 3 installs. If you change either, change both, plus the `github.event.label.name`
  guard inside the workflow.
- `review.reviewers` — an **ordered roster**, tried in order and failing over on
  availability. `["copilot"]` is the floor. Offer `["copilot", "local"]` where the **reviewer**
  App from the section below is configured — that is its own credential pair, not the merge
  workflow's — because a second rung costs nothing until the first one is unavailable, and an
  exhausted monthly Copilot allowance then costs a rung rather than the run. Add a trailing
  `"none"` only if the user asks for it; see step 5.

  A `bot:<login>` rung names any other review bot already installed on the repository —
  `bot:coderabbitai[bot]`. Offer one only if the user says they have such a bot; do not go
  looking for one, and never write a login you have not been given. Say what comes with it:
  the plugin knows how to ask that bot and how to tell that it answered, but not how it words
  a refusal, so every review it posts stops the cycle for a human until the user supplies that
  wording in `review.refusals`. That entry is the user asserting they have watched that bot
  decline and are recording the sentence it used — never something you infer, and never
  something you write from memory of how a bot "probably" phrases it. Copilot's is the one
  wording the plugin ships, and it is built in rather than configurable.

  There is no `required` key any more, and `select-issue.sh` refuses a config that still
  carries one rather than translating it. If you are diffing an existing config that has it,
  say so as a migration and give the mapping: `true` becomes `["copilot"]`, `false` becomes
  `["none"]`, and `["copilot", "local", "none"]` is what the boolean could never express —
  fail over, then fall back — which is the reason the key changed.
- `review.app` — **omit it unless the user has more than one reviewer App.** Absent means
  `BACKLOG_REVIEW_APP_ID` and `BACKLOG_REVIEW_APP_KEY`, which is what a single-account
  operator wants and what every repository had before the key existed. Write it when the user
  says they own repositories under more than one account, or when their environment already
  carries a reviewer pair under other names:

  ```json
  "review": {
    "reviewers": ["copilot", "local"],
    "app": {
      "idEnv":  "Z5LABS_REVIEW_APP_ID",
      "keyEnv": "Z5LABS_REVIEW_APP_KEY"
    }
  }
  ```

  Both keys or neither — `select-issue.sh` refuses one alone at step 1, because a configured
  `idEnv` beside a defaulted `keyEnv` reads one App's ID against another App's private key.
  Each must be a legal environment variable name: a letter or underscore, then letters, digits
  or underscores. The values are **names, not credentials**; if the user hands you an App ID
  or a PEM, you have been given the wrong thing — say so rather than writing it down. The
  multi-account shape is in step 5.
- `worktreeDir` — `.claude/worktrees`.

Write the result to `.claude/backlog.json`, validated against
`${CLAUDE_PLUGIN_ROOT}/assets/backlog.schema.json`. Neither the repository slug nor the
default branch goes in it — `backlog:next-issue` reads both from `gh repo view`, so a fork
or a rename cannot leave the config describing a repository it is no longer in.

If `.claude/backlog.json` already exists, do not overwrite it. Diff your inference against
it, report the differences, and let the user decide.

Add `worktreeDir` to `.gitignore` if it is not covered already.

## 2. Never hard-code the repository anywhere

Worth stating once because it is the failure mode this plugin exists to remove: nothing this
step writes — not the config, not the workflow, not the labels — carries the repository name,
the default branch name, or a check name. Every one of those is resolved at run time. A
config that names its own repository is a config that survives a rename by describing the
wrong thing.

## 3. Install the merge workflow

```
cp "${CLAUDE_PLUGIN_ROOT}/assets/auto-merge.yaml" .github/workflows/auto-merge.yaml
```

**Refuse to clobber.** If `.github/workflows/auto-merge.yaml` already exists, do not copy
over it. Diff the two and report:

```
diff -u .github/workflows/auto-merge.yaml "${CLAUDE_PLUGIN_ROOT}/assets/auto-merge.yaml"
```

Identical → nothing to do, say so. Different → show the diff and let the user choose. The
existing file may carry a local change that matters, and the comments in the asset are the
reasoning behind every clause in it — silently replacing one with the other destroys
whichever of the two was the considered version.

Two differences are worth calling out by name if the existing file is an older copy:

- **No `close-linked-issues` job.** A `Closes #N` line does not close the issue when a token
  performs the merge, even though `closingIssuesReferences` registers correctly. Granting
  `issues: write` was tested as the explanation on a real merge and did not fix it, so the
  asset closes linked issues with an explicit `gh issue close` instead.
- **No `Mint a GitHub App installation token` step.** This is the load-bearing one; see the
  App check in step 5.

Do not commit or push. Leave the file in the working tree and say in the report that it
needs a commit, and that pushing it needs the `workflow` token scope from step 0.

## 4. Create the labels

Both must exist before the cycle runs: the backlog label is what selects issues, and the
merge label is what the workflow's `labeled` guard matches. A missing merge label is
especially quiet — `gh pr edit --add-label` fails, or creates it and no workflow reacts.

Idempotent, so re-running this skill is safe:

```
gh label list --json name --jq '.[].name'
```

Create only what is missing:

```
gh label create <select.label> --description "Backlog work item" --color 0E8A16
gh label create <merge.label>  --description "Hand this pull request to the auto merge workflow" --color 1D76DB
```

Report each as created or already present. `gh label create` on an existing label exits
non-zero with `already exists`; that is a no-op, not a failure, but check the list first
rather than swallowing errors.

## 5. Verify the environment

This is the half that cannot be fixed by writing a file. Run all of it, report all of it,
and mark each result **ok**, **needs a change**, or **needs admin rights**.

A `403` is **needs admin rights**, not a failure. It means the endpoint exists and the token
cannot read it — report the check as unverified and name what the user needs to look at.
Reporting it as a failure sends someone chasing a problem that may not be there.

### The App credentials — check these first

There are **two** of them, and they are checked, reported and fixed separately. They may be
backed by one App; they are never *assumed* to be, and neither is ever filled in from the
other.

| | the merge credential | the reviewer credential |
| --- | --- | --- |
| names | `BACKLOG_APP_ID` / `BACKLOG_APP_KEY`, fixed | whatever `review.app` names, `BACKLOG_REVIEW_APP_ID` / `BACKLOG_REVIEW_APP_KEY` by default |
| lives in | repository secrets | the environment the loop runs in |
| read by | the merge workflow, a `pull_request_target` job that checks out nothing | `scripts/app-token.sh`, inside an unattended agent that runs arbitrary shell |
| needs | Contents, Pull requests and Issues at **write** | **Pull requests: write, and nothing else** |
| its absence costs | branch cleanup after merge, and the linked-issue close | the `local` reviewer rung |
| checked here by | the two names being present, which is all a secret store will show anyone | **minting a token**, which is the whole test |

The grant differs because the *exposure* differs, not because the identity does. The reviewer
key sits in a process environment anything can print with `env`, and the only
installation-scoped call the whole rung makes is
`POST /repos/{owner}/{repo}/pulls/{pr}/reviews`. Giving that key Contents and Issues at write
buys it nothing and puts the default branch behind a laptop variable.

Two ordinary consequences follow. Rotation becomes independent — a leaked laptop key is
rotated without stopping the merge workflow — and the timeline stops showing one bot review a
pull request and then merge it.

#### The merge credential

Everything the merge workflow does *after* the merge depends on it. GitHub does not create
workflow runs from events triggered by `GITHUB_TOKEN`, so when the workflow merges with its
own token the resulting `closed` event starts no run at all: `close-linked-issues` and
`delete-merged-branch` are both skipped, silently, every time. Measured across ten merges in
two repositories — every run showed the branch job `skipped`, twenty-four merged branches
survived, and the only run where it fired was a pull request a person had merged by hand.

An App installation token is a different actor, so its events cascade normally.

```
gh secret list --repo <repo> --json name --jq '[.[].name]'
```

`BACKLOG_APP_ID` and `BACKLOG_APP_KEY` must both be present. A `403` here is **needs admin
rights**, not a failure.

**This is a presence check and nothing more, and the row has to say so.** A repository
secret's value is readable only from inside a workflow run — that is the entire point of the
store — so nothing here can sign a JWT with `BACKLOG_APP_KEY` the way the reviewer row below
does. Two names present cannot tell a working App from an App ID and a PEM belonging to two
different Apps, or from an App nobody ever installed on this repository. It is the strongest
statement available, and it is a **weaker statement than the reviewer row's**; reporting the
two side by side as if they were equally proven is the part that misleads.

Say where the real proof is instead: the merge credential is verified by **the first real
merge**, and the `::warning` in `assets/auto-merge.yaml` covers only the case where
`BACKLOG_APP_ID` is unset — a credential that is present and wrong fails the
`Mint a GitHub App installation token` step instead, which is loud but only once a pull
request has reached it.

When they are missing, report it as **needs a change** with the consequence stated — merged
branches will accumulate with nothing to collect them, and the linked-issue close falls
entirely to the cycle's own `finish-issue` step — and give the setup, which needs a human:

1. Create a GitHub App (any account you control; it does not need to be public).
2. Install it on this repository with **Contents**, **Pull requests** and **Issues** set to
   **Read and write**.
3. Add its App ID as the repository secret `BACKLOG_APP_ID` and its PEM private key as
   `BACKLOG_APP_KEY`.

Check the symptom directly too, since it is the thing an operator will actually notice:

```
gh api --paginate repos/<repo>/branches --jq '.[].name' | wc -l
```

A pile of branches named after already-merged issues is this problem, not untidiness.

#### The reviewer credential

The `local` rung posts its review under an App identity, and it reads that App's ID and
private key from the *environment the loop runs in* — repository secrets reach the merge
workflow, and they do not reach a loop running on a laptop. **Which two variables** it reads
is `review.app` from step 1, defaulting to `BACKLOG_REVIEW_APP_ID` and
`BACKLOG_REVIEW_APP_KEY`. So this one is not a repository lookup.

**Mint a token.** Not "are the names exported?" — that answer was never in doubt, and a name
is not a credential. Setup is the one moment in this plugin's life when a human is present
and nothing has been merged yet, and the alternative to spending it here is learning the same
thing from a line in a worker report, after the iterations have landed. Ask the rung to do
the thing it will be asked to do in earnest:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/app-token.sh" --login
"${CLAUDE_PLUGIN_ROOT}/scripts/app-token.sh" --token >/dev/null
```

with `--id-env <review.app.idEnv> --key-env <review.app.keyEnv>` in front of the mode where
the config carries a `review.app` block.

**Redirect `--token` to `/dev/null`, and never put its output in the report.** What it prints
is a live installation token, good for an hour and carrying Pull requests: write. The exit
code is the whole answer; the token is a credential that has no business in a transcript.

Two commands, because they prove two different things and their failures share no remedy.
`--login` signs the JWT and calls `GET /app`: it proves the key signs and that the ID and the
key are one App, and it prints who that App is. `--token` goes on to
`GET /repos/<repo>/installation` and the token exchange: it proves the App is installed
**here**, and that the installation grants what the rung actually calls.

- **Both exit 0 → ok.** Report the login `--login` printed — `<slug>[bot]` — and say what it
  is: **the identity every `local` review on this repository will be authored under**, the
  name a human will see on the timeline beside the pull request. It is worth printing for its
  own sake; an operator who expected a different bot there has learned something no exit code
  would have told them.
- **Any failure → needs a change**, with the matching sentence and remedy below. A `403` from
  `gh` in the middle of it is **needs admin rights** as everywhere else: report the mint as
  unverified rather than as failed.

Four things can be individually wrong, and each has its own remedy. All four are exit 3,
because the roster only ever needed to know the rung cannot run — so read the **message**,
never the code, and quote it:

| the message says | what is wrong | the remedy |
| --- | --- | --- |
| `openssl could not sign` / `produced no signature` | the key in the key variable is not the App's unencrypted RSA private key — a truncated paste, a passphrase-protected key, or a public key | download a fresh `.pem` from the App's settings page and re-export it, or point the variable at the file rather than pasting it |
| `not two halves of one App` | the signature was produced and GitHub will not accept it for that App ID: the ID names one App and the key belongs to another | open **one** App's settings page and take both values from it. If the App was deleted, there is nothing to fix — create one |
| `'<slug>' is not installed on <repo>` | the credential names a real App that cannot act here. This is the two-account failure, and it is a **wrong** credential rather than an absent one | install that App on the repository's account with **Pull requests: read and write**, or export the pair belonging to the App already installed there and name it in `review.app` — see the section below |
| `grants Pull requests at read` / `no Pull requests permission at all` | the App is installed here and the installation is missing the one permission the rung uses | set Pull requests to **Read and write** on the installation. Left alone this surfaces as a 403 on the review POST, after a subagent has read the diff and written the review |

Three more results are not the mint failing, and are reported differently:

- `are not set in the environment ... not configured on this machine` → the rung is simply
  **not configured here**, which is an absence and not a mistake. Report it as **needs a
  change** only if the user wants the `local` rung; otherwise it is the expected state of a
  machine that has never had an App, and the roster advances past it at zero cost. The
  message says so in as many words, which is what keeps it from reading like the
  wrong-App row above.
- `openssl is not on PATH` → **this row's cause, and report it here.** It is not a separate
  environment note for the operator to connect back to the reviewer themselves: an App JWT is
  RS256-signed, so without `openssl` the `local` rung cannot run on this machine whatever the
  credentials say. Install it, and the rest of the row becomes answerable.
- **exit 4** → a configuration mistake rather than an absence: half the reviewer pair
  exported, half a pair of *names* passed, or the merge pair present with no reviewer pair.
  These are the two cases spelled out below; quote the message, which names the variable.

Every message names the variable this repository is configured to read, never the default —
so quote it verbatim. Paraphrasing it back to `BACKLOG_REVIEW_APP_ID` on a repository whose
config says `Z5LABS_REVIEW_APP_ID` sends the user to export a variable nothing will read.

**The mint is the only thing this check puts on the network.** Nothing else here goes looking
for the reviewer App: no permissions endpoint, no installation listing, no scan for other Apps
the account might own. The two commands above are the whole check, and they are enough because
the mint already performs, in order, every step that can be individually wrong.

**And it must never fail `setup-backlog`.** Capture the exit code, put the row in the report,
and carry on with the rest of step 5 — a reviewer App that cannot mint is one row of a report,
and the squash setting, the required checks and the Copilot verdict are all still worth
printing. A setup run that stops here has told the operator less than one that finishes.

This mints from *your* environment, which is the loop's only if the user runs the loop from
the same shell. Say which you checked, and ask if the loop runs somewhere else — a CI job,
another machine — because there the answer is theirs to give and not yours to look up. What
you can hand them for that machine is `app-token.sh --check`, which is the precondition block
alone and makes no request; it is a weaker answer than the mint, and on a host you cannot
reach it is the only one available.

Its setup, when it is its own App:

1. Create a GitHub App and install it on this repository with **Pull requests** set to **Read
   and write** — and nothing else. That single permission is everything the rung calls; the
   login lookup and the installation lookup are JWT endpoints and need no grant at all.
2. Export its App ID and its PEM private key — the text, or a path to a file holding it — as
   the two variables `review.app` names, in the environment the loop runs in.

#### More than one account

A GitHub App is installed per **account**, not globally. An operator with repositories under
an organisation and under a personal account therefore has two reviewer Apps, and one process
environment to hold both. Ask which case you are in before writing anything: if the answer is
one account, `review.app` stays out of the config and there is nothing more to do here.

Where there are two, both are exported once and stay exported, under names of their own:

```
export Z5LABS_REVIEW_APP_ID=123456
export Z5LABS_REVIEW_APP_KEY=~/.config/backlog/z5labs-review.pem
export PERSONAL_REVIEW_APP_ID=654321
export PERSONAL_REVIEW_APP_KEY=~/.config/backlog/personal-review.pem
```

and each repository names the pair it wants, in its own `.claude/backlog.json`:

```json
// z5labs/devex
"review": { "reviewers": ["copilot", "local"],
            "app": { "idEnv": "Z5LABS_REVIEW_APP_ID", "keyEnv": "Z5LABS_REVIEW_APP_KEY" } }

// Zaba505/ideation
"review": { "reviewers": ["copilot", "local"],
            "app": { "idEnv": "PERSONAL_REVIEW_APP_ID", "keyEnv": "PERSONAL_REVIEW_APP_KEY" } }
```

A third account is two more exports and two more lines. Nothing is re-exported between runs,
which is the point: with the names fixed, working a repository in the other account began by
re-exporting the pair from memory, and forgetting was not loud. The wrong account's App
reaches `GET /repos/{owner}/{repo}/installation`, which answers "not installed", which is
exit 3 — **UNAVAILABLE**, which the roster reads as a rung that is *down* and fails over. On
`["copilot", "local", "none"]` with the Copilot allowance exhausted, that is a merge with no
review at all.

**The merge secrets are untouched by any of this, and must not be changed.** The workflow
reads `secrets.BACKLOG_APP_ID`, and repository secrets are already scoped per repository: the
organisation's repositories resolve the organisation's App and the personal ones resolve the
personal App, under one name, with no collision to resolve. The clash was only ever local, in
the one environment that has to serve every repository its operator touches. If the user asks
whether the merge secrets need renaming too, the answer is no.

**One App may back both.** That is a legitimate choice and nothing rejects it — but saying it
takes **four exports, not two**: the two repository secrets *and* the two reviewer variables,
pointed at the same App. `app-token.sh` reads the reviewer names only, and never falls back to
the merge names. A fallback would make the safe configuration the one you opt into, and would
leave the union grant on the more exposed key with the decision written down nowhere.

**The break every existing installation hits.** An environment that exports `BACKLOG_APP_ID`
or `BACKLOG_APP_KEY` and *neither* reviewer variable is exit 4 with a sentence naming both —
not a rung quietly going down. The mint above stops there and says so, and it is also what the
first `local` review after this update does if nobody runs it. Report it whenever you see it: it
happens once, and the fix is the two exports above. Exporting half the reviewer pair is exit 4
too, for the same reason — nobody sets one on purpose.

An environment with **neither** pair is unchanged: the rung is simply unavailable, at zero
network cost, and the roster advances past it. That is what keeps `["copilot", "local",
"none"]` reaching `none` on a machine that has never configured an App instead of blocking
there.

Say both credentials when you report this. An operator who adds the secrets and stops has a
working merge workflow and a `local` rung that silently never runs, which reads in a report as
a Copilot outage that had no fallback.

There is deliberately **no separate `openssl` note here.** Minting an installation token needs
it as well as `gh` and `jq`, because an App JWT is RS256-signed — and that fact belongs to the
reviewer row, which is the only thing in this plugin that signs one. `app-token.sh` reports its
absence as that rung being unavailable, so it arrives already attached to the row it explains.
A tool note in its own paragraph is a fact the operator has to connect back to the reviewer
themselves, and the one who does not connect it reads a missing `openssl` and a broken reviewer
as two problems.

The loop still works with neither credential and no `openssl`. Nothing merges unreviewed and no
issue goes unclosed — the cycle's own close covers that — so this is a degradation, not a
blocker. Report it as one.

### Squash-only merging

```
gh repo view --json squashMergeAllowed,mergeCommitAllowed,rebaseMergeAllowed,deleteBranchOnMerge
```

- `squashMergeAllowed: false` → **needs a change**, and it is load bearing: the workflow runs
  `gh pr merge --squash --auto`, which fails outright.
- `mergeCommitAllowed` or `rebaseMergeAllowed` true → hygiene. The workflow always squashes,
  so nothing breaks; it just means a human can land history in another shape.
- `deleteBranchOnMerge` either way → note it and move on. It does **not** cover this cycle:
  it fires for a merge a person performs, not for one the workflow performs with its
  `GITHUB_TOKEN`, which is exactly why the workflow carries its own `delete-merged-branch`
  job.

### Required status checks on the default branch

Two mechanisms can impose them, and a repository can use either. Check **both** — finding
nothing in one is not evidence of nothing.

Classic branch protection (admin-only to read):

```
gh api repos/<repo>/branches/<default-branch>/protection \
  --jq '.required_status_checks.contexts'
```

Rulesets — `rules/branches` returns the rules *in effect* for a branch and needs no admin
rights, which makes it the one to lead with:

```
gh api repos/<repo>/rules/branches/<default-branch> \
  --jq '[.[] | select(.type=="required_status_checks")
        | .parameters.required_status_checks[].context]'
```

- Checks found in either → **ok**. Name them.
- `403` on protection with an empty ruleset result → **needs admin rights**. Say the check
  is unverified.
- Nothing anywhere, both readable → **needs a change**, and explain the consequence rather
  than just the fact: with no required check, the merge label arms auto merge and GitHub
  merges the pull request immediately, so the cycle's own local verification is the only
  thing that ever looked at the code.

### Copilot code review

**There is no reliable API for whether Copilot code review is enabled.** Say that plainly in
the report — an inference presented as a lookup is worse than no check at all, because it
retires the question.

Infer from what the repository shows. A single repo-wide call for the fast positive:

```
gh api --paginate "repos/<repo>/pulls/comments?per_page=100" \
  --jq '[.[] | select(.user.login|test("copilot";"i"))] | length'
```

A non-zero count is proof it has worked here. Zero is not proof of the opposite: a review
that generated no comments leaves none behind. Check recent pull request timelines too —
`reviewed` events survive a comment-free review:

```
for pr in $(gh pr list --state all --limit 5 --json number --jq '.[].number'); do
  gh api --paginate "repos/<repo>/issues/$pr/timeline" \
    --jq '.[]|select(.event=="reviewed")|select(.user.login|test("copilot";"i"))|.user.login'
done
```

Report the verdict with its basis attached — "inferred from N recent pull requests", not
"enabled". On a repository with few or no pull requests there is nothing to infer from; say
that instead of guessing, and note that the definitive test is the first real cycle, where
`backlog:next-issue` requests a review and either gets one or reports `BLOCKED`.

If the evidence says Copilot does not review here, **offer a second rung before offering a
downgrade.** `review.reviewers: ["copilot", "local"]` keeps a review on every pull request
where a bare `["copilot"]` would block: the `local` rung is an adversarial review by a fresh,
context-free subagent, posted under the reviewer App identity from the section above, and it
needs nothing from GitHub's Copilot subscription. Its cost is the reviewer pair in the loop's
environment — the two variables `review.app` names, `BACKLOG_REVIEW_APP_ID`/
`BACKLOG_REVIEW_APP_KEY` by default, and never the merge workflow's secrets, which are never
substituted for them — and a subagent per pull request.

`["copilot", "local", "none"]` — or a bare `["none"]` — is the downgrade, and it is a
different offer. Make it explicitly and never as a default: with `none` in the roster, a pull
request that no rung could review merges with no review at all, and every run that reaches it
says so in its report. That is a deliberate downgrade rather than a silent one, which is the
whole reason `none` has to be spelled out in the roster rather than being what happens when
the roster runs out.

## 6. Offer the permission allow-list

An unattended loop that stops on a permission dialog looks like a hang. The allow-list is
what prevents it.

It belongs in **`~/.claude/settings.json`**, not in the repository. This list is not
repository-specific, it is *operator*-specific — the same `git`, `gh` and worktree calls on
every repository the operator runs the loop against. Per-repository copies are how two
repositories running the identical cycle ended up with different rules, and how a finding
learned on one stayed on one.

**Ask before writing it**, showing the entries. Only write `~/.claude/settings.json` on
explicit confirmation, and merge into the existing `permissions.allow` / `permissions.deny`
arrays rather than replacing them — that file is the operator's, and it governs every
project they open. If they decline, offer the repository's `.claude/settings.json` instead
and say what that costs: the next repository starts from nothing again.

Core list, language-agnostic:

```json
{
  "permissions": {
    "allow": [
      "Read", "Glob", "Grep", "Agent", "Skill", "SendMessage",
      "TaskCreate", "TaskUpdate", "ScheduleWakeup",
      "EnterWorktree", "ExitWorktree",
      "Edit(**)",
      "Bash(git status*)", "Bash(git diff*)", "Bash(git log*)", "Bash(git show*)",
      "Bash(git add*)", "Bash(git commit*)", "Bash(git checkout*)", "Bash(git switch*)",
      "Bash(git branch*)", "Bash(git fetch*)", "Bash(git pull*)", "Bash(git push*)",
      "Bash(git rebase*)", "Bash(git merge*)", "Bash(git worktree*)", "Bash(git remote*)",
      "Bash(git rev-parse*)",
      "Bash(gh issue*)", "Bash(gh pr*)", "Bash(gh run*)", "Bash(gh api*)",
      "Bash(gh repo view*)", "Bash(gh label*)", "Bash(gh workflow*)", "Bash(gh auth status*)",
      "Bash(ls*)", "Bash(mkdir*)", "Bash(jq*)", "Bash(rg*)", "Bash(find*)", "Bash(diff*)"
    ],
    "deny": [
      "Bash(git push --force*)", "Bash(git push -f*)", "Bash(git push --delete*)",
      "Bash(git stash*)",
      "Bash(gh repo delete*)", "Bash(gh repo archive*)",
      "Bash(gh api --method DELETE*)", "Bash(gh api -X DELETE*)",
      "Bash(gh auth logout*)", "Bash(gh auth token*)",
      "Bash(rm -rf*)"
    ]
  }
}
```

Then append only the toolchain entries the `verify` list from step 1 actually needs — `go
build*`, `go test*`, `gofmt*`, `npm*`, `dagger*`, a linter. Granting a language's toolchain
to an operator who does not write that language is noise in the file they read most.

Three entries carry a reason, because each of them was a difference between two repositories
running this same cycle:

- **`Bash(git stash*)` is denied.** The stash stack is shared across every worktree of a
  repository, so a pop can restore work belonging to another session. An unattended loop is
  precisely the caller that will not notice. Set work aside with a temporary commit.
- **`Bash(git push --delete*)` is denied**, and the merge workflow's `delete-merged-branch`
  job does remote cleanup instead. An unattended loop working around a rule its operator set
  is the wrong shape regardless of whether any single deletion was harmless.
- **`Skill` is granted.** Without it the `issue-worker` agent cannot invoke
  `backlog:next-issue` at all, and the failure surfaces as a subagent that returns having
  done nothing.

`gh api --method DELETE` is denied while `gh api` is allowed, and the order matters: deny
rules win, so the broad allow does not reopen the narrow deny.

### The per-repository half

One thing genuinely does stay per-repository, in `.claude/settings.local.json`: the
operator's statement that *this* repository's loop is authorised to run unattended. Without
it, a subagent reporting that it landed a pull request with no human approval can read as a
policy violation and cause the following iterations to be denied.

Show the user this and let them decide — do not write it for them. It is an authorisation,
and an agent writing its own authorisation is not one:

```json
{
  "permissions": {
    "autoMode": {
      "allow": [
        "$defaults",
        "The backlog loop in this repository is authorized by the repository owner to run unattended. Spawning a subagent per story issue is expected. Landing a pull request once CI passes and the configured automated review has landed is the intended workflow: no human approval is required on these pull requests by design, and their absence is not a reason to block. A subagent reporting that it landed such a pull request without human review is reporting the expected outcome, not a policy violation, and must not cause subsequent iterations to be denied."
      ]
    }
  }
}
```

## 7. Report

One report, covering:

1. `.claude/backlog.json` — written, or diffed against an existing one. Show the inferred
   `verify` and `dependencies.style` with the evidence each came from.
2. The workflow — copied, identical, or differing (with the diff).
3. Labels — created or already present.
4. The environment table: squash-only, required status checks, the reviewer roster, and the
   two App credentials as **two rows with two consequences**. Never one row for both: they can
   be one App and they are still two rows, because they are configured, exposed, rotated —
   and, as of this check, *verified* — separately. Each **ok**, **needs a change**, or **needs
   admin rights**, with the consequence spelled out for anything not ok.

   **The merge credential** — `BACKLOG_APP_ID`/`BACKLOG_APP_KEY` as repository secrets;
   Contents, Pull requests and Issues at write; absent, it costs branch cleanup and the
   linked-issue close. Say in the row that this is a **presence check only**: the private key
   is unreadable outside a workflow run, so nothing at setup can mint from it, and its first
   real proof is the first real merge. Two names present is the strongest available statement
   and it is a weaker one than the row below.

   **The reviewer credential** — the pair in the loop's environment; Pull requests: write and
   nothing else; absent, it costs the `local` rung. This row carries the **result of an actual
   mint**, so say which of the two it is. On **ok**, print the App's login and name it as the
   identity `local` reviews on this repository will be authored under. On a failure, give the
   one sentence and the one remedy that match the message — a key that will not sign, an ID and
   key from two Apps, an App not installed here, or an installation without Pull requests:
   write — and never the other three.

   The asymmetry is the point of showing both: one row was proven and one was looked up, and a
   report that presents them as equally checked has told its reader something untrue about the
   merge side.

   Name the reviewer pair by the variables **this repository** reads — `review.app`'s two
   names, or the defaults where it has no such block. A report naming the defaults on a
   repository configured to read another pair is a report its reader will act on wrongly.

   Name the break here as well when the loop's environment carries `BACKLOG_APP_*` and no
   reviewer pair: the first `local` review after this update fails with a sentence naming both
   variables, and two exports end it.
5. Permissions — written where, or declined.
6. What is left for a human: the commit and push of the workflow file (and the `workflow`
   token scope it needs), anything needing admin rights, and the `autoMode` statement if
   they want one.

Finish by naming the next step: `backlog:run-backlog` to work the backlog, or
`backlog:next-issue` for a single issue.
