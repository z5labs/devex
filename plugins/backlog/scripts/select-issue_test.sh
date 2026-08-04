#!/usr/bin/env bash
#
# select-issue_test.sh — the fixture corpus for select-issue.sh's extraction.
#
# Every case below is an issue body that a real backlog can contain. The ones
# marked REGRESSION were extracted wrongly by the awk/sed/grep pipelines this
# script replaced, back when they lived in `skills/next-issue/SKILL.md` as prose
# for an agent to retype and check against a table by eye. Three of them
# produced a *silently eligible* issue, which is the failure that gets work done
# in the wrong order rather than not at all.
#
# Run: plugins/backlog/scripts/select-issue_test.sh
# Exit 0 when every case matches, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/select-issue.sh"
[ -x "$SUT" ] || { printf 'select-issue.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

pass=0
fail=0

# check <name> <style> <expected refs, space separated> <body>
check() {
  local name=$1 style=$2 want=$3 body=$4 got
  got=$(printf '%s' "$body" | "$SUT" --extract "$style" | tr '\n' ' ')
  got=${got% }
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1))
    printf '  ok   %-58s [%s]\n' "$name" "${got:-<none>}"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s want [%s] got [%s]\n' "$name" "$want" "$got"
  fi
}

printf '\nblocked-by\n'

# The fixture the skill has always documented. The three styles must give three
# different answers on this one body.
check 'the documented fixture' blocked-by '#12 #14' \
'### Related Issues

Blocked by:
- #12 — the parser this builds on
- #14

Depends on #17.

- #19 is related but not blocking.'

# REGRESSION: `tolower($0) ~ /blocked by/` was unanchored, so a sentence saying
# the issue is NOT blocked opened a dependency list and swallowed the next
# bullet. The issue was then blocked forever on a dependency not in its body.
check 'REGRESSION negated prose does not open a list' blocked-by '' \
'This issue is not blocked by anything.

- #77 see also'

# REGRESSION: same root cause, past tense, in a paragraph nowhere near the
# Related Issues section.
check 'REGRESSION incidental prose does not open a list' blocked-by '' \
'Background

The old parser was blocked by a flaky test.

- #99 unrelated tracking bullet'

# REGRESSION: an issue template quoted inside a fence is an example, not a
# declaration.
check 'REGRESSION fenced code block is not a declaration' blocked-by '' \
'Example config:

```
Blocked by:
- #123
```

real text'

# REGRESSION: the inline form extracted nothing at all, so the issue read as
# unblocked. Every reference on the label line counts — there is no gloss
# convention there, and taking only the first would drop #14.
check 'REGRESSION inline form on the label line' blocked-by '#12 #14' \
'Blocked by: #12, #14

Some prose.'

# REGRESSION: GitHub renders `- [ ] #12` as a task list. The old list-item
# regex required `#` immediately after the bullet, so a checklist of blockers
# extracted nothing.
check 'REGRESSION task-list checkboxes' blocked-by '#12 #14' \
'Blocked by:
- [ ] #12
- [x] #14'

# REGRESSION, and the worst of them. `- z5labs/other#42` was not a list item
# under the old regex, so it TERMINATED the list — the cross-repo reference was
# dropped AND the real in-repo dependency below it was never seen. Now both
# survive, and the caller reports BLOCKED on the unresolvable one.
check 'REGRESSION cross-repo item keeps the list open' blocked-by 'z5labs/other#42 #14' \
'Blocked by:
- z5labs/other#42
- #14'

check 'a markdown heading ends the list' blocked-by '#12' \
'Blocked by:
- #12

## Acceptance Criteria

- [ ] parser handles #500 style refs
- #501'

check 'prose ends the list' blocked-by '#12' \
'Blocked by:
- #12
Some following paragraph.
- #34'

check 'bold label' blocked-by '#12' \
'**Blocked by:**
- #12'

check 'heading label with no colon' blocked-by '#12' \
'### Blocked by

- #12'

# The first reference on an item is the dependency; the rest of the line is a
# gloss. This is the rule the documented fixture depends on.
check 'first ref per item, gloss ignored' blocked-by '#12' \
'Blocked by:
- #12 — needs #34 first'

# Markdown calls this one loose list, so both items are dependencies. Ending at
# the blank line instead would silently drop #19, and a missed dependency is
# worse than an extra one: the extra one halts the loop with a visible reason.
check 'a blank line does not end a loose list' blocked-by '#12 #19' \
'Blocked by:
- #12

- #19'

check 'duplicates collapse, order preserved' blocked-by '#14 #12' \
'Blocked by:
- #14
- #12
- #14'

check 'no label at all' blocked-by '' \
'Just an ordinary issue body with a reference to #5 in it.'

printf '\ndepends-on\n'

check 'the documented fixture' depends-on '#17' \
'### Related Issues

Blocked by:
- #12 — the parser this builds on
- #14

Depends on #17.

- #19 is related but not blocking.'

# REGRESSION: `grep -oiE '"'"'depends on[[:space:]]*#[0-9]+'"'"'` matched a
# cross-repo reference nowhere, so the issue came out eligible with an
# unresolved dependency.
check 'REGRESSION cross-repo reference is reported' depends-on 'z5labs/other#42' \
'Depends on z5labs/other#42'

# REGRESSION: a note that a dependency was removed is not a dependency.
check 'REGRESSION no longer depends on' depends-on '' \
'This no longer depends on #17 since #20 landed.'

check 'REGRESSION never depends on' depends-on '' \
'This never depends on #17.'

check 'REGRESSION fenced code block is not a declaration' depends-on '' \
'For example:

```
Depends on #123
```'

check 'colon form' depends-on '#17' 'Depends on: #17'

check 'case insensitive' depends-on '#17' 'depends on #17'

check 'several, deduped in order' depends-on '#17 #20' \
'Depends on #17 for the schema.

Also depends on #20, and depends on #17 again.'

# The reference has to follow the phrase. A sentence that merely mentions an
# issue later on is not declaring a dependency on it.
check 'non-adjacent reference is not a dependency' depends-on '' \
'Depends on the parser landing, see #99 for context.'

check 'blocked-by list items carry no depends-on phrase' depends-on '' \
'Blocked by:
- #12
- #14'

printf '\nnone\n'

check 'parses nothing' none '' \
'Blocked by:
- #12

Depends on #17.'

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
