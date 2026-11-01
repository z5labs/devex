#!/usr/bin/env bash
#
# scan-plugin.sh — say, per hosted agent target, which of a Claude Code plugin's
# components will work there, which need setup and which will fail.
#
# A plugin that works on a laptop can fail in a hosted session, and nothing tells
# its author until someone runs it there. What each hosted target changes lives
# in data, assets/targets/<id>.json, one profile per target, every rule with its
# sources, the basis it rests on and the date it was last checked. This script
# applies those rules. It makes no network calls; it reads the plugin, the
# repository the plugin is delivered from, and the profiles.
#
# What it reads, per component:
#   - skills, commands and agents: their frontmatter's tool fields, their fenced
#     shell blocks (no info string, or sh/bash/shell/zsh; in a console or
#     shell-session block, only the `$ ` lines), and the Agent(subagent_type: …)
#     spawns in those blocks;
#   - hooks: every hook entry of every type, from hooks/hooks.json or the
#     manifest; a command hook's command and an http hook's URL are matched;
#   - scripts: every line that is not a comment, anywhere in the plugin except
#     assets/, tests and test data. Shell is read as shell; Python, JavaScript,
#     Ruby and Perl keep their strings, where their commands are;
#   - .mcp.json and .lsp.json servers, and the manifest's own declarations.
# The manifest itself is optional, as it is for Claude Code: without one, the
# plugin is the standard layout it finds, named after its directory. A
# component file that is not valid JSON is an error (exit 2), not an absence.
#
# Commands are matched only where they run. A skill whose prose says "never run
# `gh pr merge`" is not a finding; the same command in a fenced shell block or a
# script is. On a shell line, quoted text, message commands, trailing comments
# and heredoc bodies are not commands; AWK_LIB below says exactly how.
#
# Where a command runs can also set how bad it is: a rule's severity_in gives a
# fenced block, where the model sees the failure and can adapt, a lower severity
# than a script, where the failure is an exit code. For a tool, the context is
# frontmatter where it is declared and fence where a fenced block spawns it: a
# declaration says a component may use the tool, a spawn that its flow does.
#
# A script can already handle what a rule describes: test CLAUDE_CODE_REMOTE and
# skip the step, or recognise the refusal by its message and exit with one that
# names it. A command rule's guard says how that reads: when another line of code
# in the same script matches the guard's pattern, every match of the rule in that
# script is rated at the guard's severity instead of the script severity. The
# guard covers the whole file, and only scripts; a fence, a hook or frontmatter
# keeps its severity. It is matched with the line's quoted text kept, so a grep
# for the message counts, but a comment, a message command or a heredoc's text is
# not code and guards nothing.
#
# Components call each other, and a verdict follows the call: a skill that runs
# scripts/x.sh in a fenced block, a script that runs another by name, a fenced
# Agent(subagent_type: "<agent>") spawn, a fenced mention of <plugin>:<skill>,
# and an agent's `skills:` frontmatter are edges, and a component's verdict
# counts what it reaches. An invocation written only in prose ("invoke the
# backlog:next-issue skill") is not an edge; the review-plugin skill reads for
# those.
#
# Delivery comes first. Each profile's delivery rules decide, from the
# repository, whether the plugin reaches a session on that target at all. A
# plugin that cannot be delivered is `breaks` for every component. A repository
# that commits only the plugin's skills, agents and commands delivers only
# those, and the profile's vendored rules report what that leaves behind. With
# no repository (--no-repo, or a plugin outside any git work tree), delivery is
# not assessed and the report says so.
#
# Usage:
#   scan-plugin.sh <plugin-dir> [--target <id>]... [--repo <dir> | --no-repo]
#                  [--review <findings.json>] [--format json|markdown]
#                  [--targets-dir <dir>]
#
#   --target       a profile id; repeat for several. Default: every profile.
#   --repo         the repository the plugin is delivered from. Default: the git
#                  work tree that contains the plugin, if any.
#   --review       a JSON array of findings the review-plugin skill made by
#                  reading prose. They are merged in with basis "review"; each
#                  must cite a rule of its target's profile. They can only add:
#                  nothing here drops or downgrades a scanned finding.
#   --format       json (default): one JSON document. markdown: the report.
#
# Exit codes:
#   0  no finding is `breaks`
#   1  at least one finding is `breaks`
#   2  the input is not a plugin, a profile cannot be read, or the arguments or
#      the review file are wrong

set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TARGETS_DIR="$HERE/../assets/targets"

die() { printf 'scan-plugin: %s\n' "$*" >&2; exit 2; }
usage() { sed -n '/^# Usage:/,/^# Exit codes:/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' | sed '$d'; }

command -v jq >/dev/null 2>&1 || die "jq is required"

PLUGIN="" REPO="" REPO_MODE=auto REVIEW="" FORMAT=json
TARGETS=()
while [ $# -gt 0 ]; do
  case $1 in
    --target)      [ $# -ge 2 ] || die "--target needs a profile id"; TARGETS+=("$2"); shift 2 ;;
    --target=*)    TARGETS+=("${1#*=}"); shift ;;
    --repo)        [ $# -ge 2 ] || die "--repo needs a directory"; REPO=$2; REPO_MODE=given; shift 2 ;;
    --repo=*)      REPO=${1#*=}; REPO_MODE=given; shift ;;
    --no-repo)     REPO_MODE=none; shift ;;
    --review)      [ $# -ge 2 ] || die "--review needs a file"; REVIEW=$2; shift 2 ;;
    --review=*)    REVIEW=${1#*=}; shift ;;
    --format)      [ $# -ge 2 ] || die "--format needs json or markdown"; FORMAT=$2; shift 2 ;;
    --format=*)    FORMAT=${1#*=}; shift ;;
    --targets-dir) [ $# -ge 2 ] || die "--targets-dir needs a directory"; TARGETS_DIR=$2; shift 2 ;;
    -h|--help)     usage; exit 0 ;;
    -*)            die "unknown option $1 (see --help)" ;;
    *)             [ -z "$PLUGIN" ] || die "one plugin directory at a time"; PLUGIN=$1; shift ;;
  esac
done
case $FORMAT in json|markdown) ;; *) die "--format is json or markdown, not $FORMAT" ;; esac
[ -n "$PLUGIN" ] || die "usage: scan-plugin.sh <plugin-dir> [--target <id>]... (see --help)"

# --- the plugin -----------------------------------------------------------------

[ -d "$PLUGIN" ] || die "$PLUGIN is not a plugin: not a directory"
P=$(cd "$PLUGIN" && pwd)
# The manifest is optional: without one, Claude Code loads the components it
# finds in the standard layout and names the plugin after its directory.
MANIFEST="$P/.claude-plugin/plugin.json"
if [ -f "$MANIFEST" ]; then
  NAME=$(jq -er 'if type == "object" and (.name | type) == "string" and (.name | length) > 0 then .name else error("no name") end' "$MANIFEST" 2>/dev/null) \
    || die "$PLUGIN is not a plugin: .claude-plugin/plugin.json is not a JSON object with a name"
  VERSION=$(jq -r '.version // "" | tostring' "$MANIFEST")
else
  layout=false
  for d in skills commands agents; do [ -d "$P/$d" ] && layout=true; done
  for f in hooks/hooks.json .mcp.json .lsp.json; do [ -f "$P/$f" ] && layout=true; done
  $layout || die "$PLUGIN is not a plugin: no .claude-plugin/plugin.json, and none of skills/, commands/, agents/, hooks/hooks.json, .mcp.json or .lsp.json"
  MANIFEST="" NAME=$(basename "$P") VERSION=""
fi

WORK=$(mktemp -d) || die "cannot create a temporary directory"
trap 'rm -rf "$WORK"' EXIT

# --- the profiles ---------------------------------------------------------------

[ -d "$TARGETS_DIR" ] || die "cannot read profiles: $TARGETS_DIR is not a directory"
TARGETS_DIR=$(cd "$TARGETS_DIR" && pwd)
if [ ${#TARGETS[@]} -eq 0 ]; then
  for f in "$TARGETS_DIR"/*.json; do
    [ -e "$f" ] && TARGETS+=("$(basename "$f" .json)")
  done
  [ ${#TARGETS[@]} -gt 0 ] || die "cannot read profiles: none in $TARGETS_DIR"
fi
: >"$WORK/profiles.ndjson"
for t in "${TARGETS[@]}"; do
  case $t in *[!a-z0-9-]*|"") die "cannot read profile '$t': not a profile id" ;; esac
  f="$TARGETS_DIR/$t.json"
  [ -f "$f" ] || die "cannot read profile '$t': no $f"
  jq -ce --arg id "$t" --arg file "$f" \
    'if type == "object" and .id == $id and (.rules | type) == "array" then . + {_file: $file} else error("bad") end' \
    "$f" >>"$WORK/profiles.ndjson" 2>/dev/null \
    || die "cannot read profile '$t': $f is not a profile whose id is '$t'"
done
jq -s '.' "$WORK/profiles.ndjson" >"$WORK/profiles.json"

# Every rule must be usable as data and must say where it comes from. A pattern
# that does not compile would otherwise fail in the middle of a scan.
problems=$(jq -r '
  def has_item($x): any(.[]; . == $x);
  def ok_date: type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$");
  def compiles: . as $p | (try ("" | test($p) | "ok") catch "bad") == "ok";
  .[] | . as $p | ($p.rules | map(.id)) as $ids
  | (if ($ids | length) != ($ids | unique | length) then "\($p.id): duplicate rule ids" else empty end),
    ($p.rules[] | . as $r | "\($p.id): rule \($r.id // "?")" as $where
      | (if ($r.id | type) != "string" then "\($where): no id" else empty end),
        (if (["breaks", "needs-setup", "degrades", "info"] | has_item($r.severity)) then empty else "\($where): bad severity" end),
        (if (["docs", "observed", "unconfirmed"] | has_item($r.basis)) then empty else "\($where): bad basis" end),
        (if (["plugin", "repository", "environment"] | has_item($r.fix_in)) then empty else "\($where): bad fix_in" end),
        (if ($r.sources | type) == "array" and ($r.sources | length) > 0 then empty else "\($where): no sources" end),
        (if ($r.checked | ok_date) then empty else "\($where): no checked date" end),
        (if ($r.remedy | type) == "string" then empty else "\($where): no remedy" end),
        ($r.match.kind as $k
          | if (["command", "url", "env", "tool", "component", "delivery", "vendored", "review"] | has_item($k)) | not then "\($where): unknown match kind \($k)"
            elif $k == "command" and (($r.match.pattern | type) != "string" or ($r.match.pattern | compiles | not)) then "\($where): pattern does not compile"
            elif $k == "env" and (($r.match.name_pattern | type) != "string" or ($r.match.name_pattern | compiles | not)) then "\($where): name_pattern does not compile"
            elif $k == "vendored" and ($r.match.pattern != null) and (($r.match.pattern | type) != "string" or ($r.match.pattern | compiles | not)) then "\($where): pattern does not compile"
            elif $k == "vendored" and ($r.match.pattern == null) and (($r.match.types | type) != "array") then "\($where): needs a pattern or types"
            elif ($k == "command" or $k == "url" or $k == "env") and ($r.match["in"] | type) != "array" then "\($where): no contexts"
            elif $k == "url" and ($r.match.allow | type) != "array" then "\($where): no allow list"
            elif $k == "tool" and ($r.match.name_pattern // "x" | compiles | not) then "\($where): name_pattern does not compile"
            elif $k == "tool" and ($r.match.ignore_pattern // "x" | compiles | not) then "\($where): ignore_pattern does not compile"
            else empty end),
        (($r.match.except // [])[] | . as $e | select($ids | has_item($e) | not) | "\($where): except names unknown rule \($e)"),
        (if $r.guard == null then empty
         elif ($r.guard | type) != "object" then "\($where): guard is not an object"
         elif $r.match.kind != "command" then "\($where): a guard needs a command match"
         elif (($r.match["in"] | type) != "array") or ($r.match["in"] | has_item("script") | not) then "\($where): a guard applies in scripts, and this rule does not match there"
         elif (($r.guard.pattern | type) != "string") or ($r.guard.pattern | compiles | not) then "\($where): guard pattern does not compile"
         elif (["breaks", "needs-setup", "degrades", "info"] | has_item($r.guard.severity)) | not then "\($where): bad guard severity"
         else empty end)
    )
' "$WORK/profiles.json" 2>&1) || die "cannot read profiles: $problems"
[ -z "$problems" ] || die "cannot read profiles: $(printf '%s' "$problems" | paste -sd ';' - | sed 's/;/; /g')"

# --- the repository it is delivered from -----------------------------------------

case $REPO_MODE in
  auto)  REPO=$(git -C "$P" rev-parse --show-toplevel 2>/dev/null) || REPO="" ;;
  given) [ -d "$REPO" ] || die "--repo $REPO is not a directory"; REPO=$(cd "$REPO" && pwd) ;;
  none)  REPO="" ;;
esac
if [ -n "$REPO" ] && [[ "$P/" == "$REPO/"* ]]; then BASE=$REPO; else BASE=$(dirname "$P"); fi
rel() { printf '%s' "${1#"$BASE"/}"; }
prel() { printf '%s' "${1#"$P"/}"; }

# --- components -----------------------------------------------------------------

: >"$WORK/comps.ndjson" ; : >"$WORK/lines.ndjson" ; : >"$WORK/hooks.ndjson"

fm_name() { # the frontmatter's name:, if any
  awk 'NR == 1 && !/^---[ \t]*$/ { exit } NR > 1 && /^---[ \t]*$/ { exit }
       NR > 1 && /^name[ \t]*:/ { v = $0; sub(/^name[ \t]*:[ \t]*/, "", v); gsub(/["'\'']/, "", v); sub(/[ \t]+$/, "", v); print v; exit }' "$1"
}

# What the rules match on a shell line is its code, reduced in four steps:
#   - a quoted string with a space in it and no command substitution is text (a
#     message, a jq program, a title): its content is dropped, but the $VAR and
#     ${VAR} references in a double-quoted one stay, because the shell expands
#     them. A string handed to bash -c, sh -c, eval or ssh is a command and stays;
#   - the line splits on unquoted |, ||, && and ;, and a part whose first word is
#     a message command (echo, printf, fail, die, say, …) is dropped, so
#     `printf … | gh issue comment` keeps the gh and `cmd || fail "gh … failed"`
#     keeps only the cmd;
#   - an unquoted # that starts a word ends the line;
#   - a heredoc's body is skipped unless the heredoc feeds a shell or an
#     interpreter (bash, sh, ssh, python, node, …).
# A line left with no code is marked as a message.
AWK_LIB='
function vars_of(s,   out, t) {
  out = ""
  while (match(s, /\$\{[!#]?[A-Za-z_][A-Za-z0-9_]*|\$[A-Za-z_][A-Za-z0-9_]*/)) {
    t = substr(s, RSTART, RLENGTH); if (substr(t, 2, 1) == "{") t = t "}"
    out = (out == "" ? t : out " " t)
    s = substr(s, RSTART + RLENGTH)
  }
  return out
}
function shell_arg(pre) {
  return (pre ~ /(^|[^A-Za-z0-9_.\/-])(bash|sh|zsh|dash|ksh)([ ]+-[A-Za-z]+)*[ ]+-[A-Za-z]*c[A-Za-z]*[ ]+$/ \
       || pre ~ /(^|[^A-Za-z0-9_.\/-])eval[ ]+$/ \
       || pre ~ /(^|[^A-Za-z0-9_.\/-])ssh([ ]+[^ ]+)+[ ]+$/)
}
function blank(s,   out, i, n, c, q, seg, bt) {
  out = ""; q = ""; seg = ""; n = length(s)
  for (i = 1; i <= n; i++) {
    c = substr(s, i, 1)
    if (q == "") {
      if (c == "\\") { out = out c substr(s, i + 1, 1); i++; continue }
      if (c == "\"" || c == "\047") { q = c; seg = ""; continue }
      out = out c
    } else {
      if (q == "\"" && c == "\\") { seg = seg c substr(s, i + 1, 1); i++; continue }
      if (c == q) {
        bt = seg; gsub(/\\`/, "", bt)
        if (shell_arg(out) || !index(seg, " ") || (q == "\"" && (index(seg, "$(") || index(bt, "`")))) out = out q seg q
        else if (q == "\"") out = out q vars_of(seg) q
        else out = out q q
        q = ""; continue
      }
      seg = seg c
    }
  }
  if (q != "") out = out q seg
  return out
}
function says_first(seg,   u, changed) {
  u = seg; sub(/^[ ]+/, "", u); changed = 1
  while (changed) {
    changed = 0
    if (sub(/^(then|else|elif|do|if|while|until|time|!|\{|\()[ ]+/, "", u)) changed = 1
    else if (u ~ /^[^ ()]+\)[ ]/) { sub(/^[^ ()]+\)[ ]+/, "", u); changed = 1 }
  }
  return (u ~ /^(echo|printf|say|fail|die|warn|err|error|log|info|note|usage|print)([ (]|$)/)
}
function code_of(b,   n, i, c, q, seg, k, segs, out, prev, t) {
  n = length(b); q = ""; seg = ""; k = 0; prev = " "
  for (i = 1; i <= n; i++) {
    c = substr(b, i, 1)
    if (q != "") {
      seg = seg c
      if (c == "\\" && q == "\"") { seg = seg substr(b, i + 1, 1); i++ }
      else if (c == q) q = ""
      prev = c; continue
    }
    if (c == "\\") { seg = seg c substr(b, i + 1, 1); i++; prev = "x"; continue }
    if (c == "\"" || c == "\047") { q = c; seg = seg c; prev = c; continue }
    if (c == "#" && (prev == " " || prev == "(")) break
    if (c == "|" || c == ";" || (c == "&" && substr(b, i + 1, 1) == "&")) {
      segs[++k] = seg; seg = ""
      if (substr(b, i + 1, 1) == c) i++
      prev = " "; continue
    }
    seg = seg c; prev = c
  }
  segs[++k] = seg
  out = ""
  for (i = 1; i <= k; i++) {
    t = segs[i]; sub(/^[ ]+/, "", t); sub(/[ ]+$/, "", t)
    if (t == "" || (says_first(t) && !index(t, "$(") && !index(t, "`"))) continue
    out = (out == "" ? t : out " ; " t)
  }
  return out
}
function heredoc(t,   p, w, pre) {
  p = t
  while (match(p, /<<-?[ ]*["\047]?[A-Za-z_][A-Za-z0-9_]*["\047]?/)) {
    if (RSTART > 1 && substr(p, RSTART - 1, 1) == "<") { p = substr(p, RSTART + RLENGTH); continue }
    w = substr(p, RSTART, RLENGTH); pre = substr(p, 1, RSTART - 1)
    sub(/^<<-?[ ]*["\047]?/, "", w); sub(/["\047]$/, "", w)
    HD_END = w
    HD_RUN = (pre ~ /(^|[ ;|&(])(bash|sh|zsh|dash|ksh|ssh|python[0-9.]*|node|ruby|perl)([ ]|$)/)
    return 1
  }
  return 0
}
function trim(x) { sub(/^[ ]+/, "", x); sub(/[ ]+$/, "", x); return x }
'

# Run contexts of a Markdown component: tool fields of its frontmatter and the
# lines of its fenced shell blocks. In a console or shell-session block only the
# `$ ` lines are commands; the rest is output.
# Output: ctx \t line \t key \t msg \t code \t literal \t original line, where
# literal is the code with its quoted text kept.
md_lines() {
  awk "$AWK_LIB"'
    BEGIN { fm = 0; inf = 0; key = ""; inhd = 0 }
    { gsub(/\t/, "    "); sub(/\r$/, "") }
    NR == 1 && /^---[ ]*$/ { fm = 1; next }
    fm == 1 {
      if ($0 ~ /^---[ ]*$/) { fm = 2; next }
      if ($0 ~ /^[A-Za-z0-9_-]+[ ]*:/) { key = $0; sub(/[ ]*:.*$/, "", key) }
      if (key == "allowed-tools" || key == "tools" || key == "skills" || key == "hooks" || key == "mcp-servers" || key == "mcpServers")
        print "frontmatter\t" NR "\t" key "\t0\t" $0 "\t" $0 "\t" $0
      next
    }
    {
      s = $0; sub(/^[ ]*/, "", s); c = substr(s, 1, 1)
      if (!inf) {
        if ((c == "`" || c == "~") && substr(s, 1, 3) == c c c) {
          n = 0; while (substr(s, n + 1, 1) == c) n++
          info = substr(s, n + 1)
          if (c == "`" && index(info, "`") > 0) next
          sub(/^[ ]+/, "", info); sub(/[ {].*$/, "", info); info = tolower(info)
          fchar = c; flen = n; inf = 1; inhd = 0
          console = (info == "console" || info == "shell-session" || info == "sh-session")
          run = (info == "" || info == "sh" || info == "bash" || info == "shell" || info == "zsh" || console)
        }
        next
      }
      if (c == fchar) {
        n = 0; while (substr(s, n + 1, 1) == fchar) n++
        rest = substr(s, n + 1)
        if (n >= flen && rest ~ /^[ ]*$/) { inf = 0; inhd = 0; next }
      }
      if (!run) next
      if (inhd) { if (trim(s) == HD_END) { inhd = 0; next } if (!HD_RUN) next }
      line = s
      if (console) { if (line !~ /^\$ /) next; line = substr(line, 3) }
      if (line == "" || line ~ /^#/) next
      code = code_of(blank(line))
      if (!inhd && heredoc(line)) inhd = 1
      print "fence\t" NR "\t\t" (code == "" ? 1 : 0) "\t" code "\t" code_of(line) "\t" $0
    }' "$1"
}

# Lines of a script that are not comments. Shell scripts are reduced as
# described above. In Python, JavaScript, Ruby and Perl a command is a string
# passed to a process call, so strings are kept; comments, docstrings and lines
# that only print or raise are left out, and a list form such as
# ["gh", "issue", "list"] reads as gh issue list.
# Output: ctx \t line \t key \t msg \t code \t literal \t original line. A shell
# line's literal is its code with the quoted text kept: no comment, no message
# command, no heredoc text. Another language's is the line as written.
script_lines() {
  local lang=sh
  case $1 in
    *.js|*.mjs|*.cjs|*.ts) lang=js ;;
    *.py) lang=py ;;
    *.rb) lang=rb ;;
    *.pl) lang=pl ;;
    *) head -n 1 "$1" 2>/dev/null | grep -Eq '^#!.*\b(python[0-9.]*|node|deno|bun|ruby|perl)\b' \
         && lang=$(head -n 1 "$1" | sed -E 's/.*\b(python[0-9.]*|node|deno|bun|ruby|perl)\b.*/\1/; s/^python.*/py/; s/^(node|deno|bun)$/js/; s/^ruby$/rb/; s/^perl$/pl/') ;;
  esac
  awk -v lang="$lang" "$AWK_LIB"'
    BEGIN { inhd = 0; inblock = 0 }
    { gsub(/\t/, "    "); sub(/\r$/, ""); t = trim($0) }
    lang == "sh" {
      if (inhd) { if (t == HD_END) { inhd = 0; next } if (!HD_RUN) next }
      if (t == "" || t ~ /^#/) next
      code = code_of(blank(t))
      if (!inhd && heredoc(t)) inhd = 1
      print "script\t" NR "\t\t" (code == "" ? 1 : 0) "\t" code "\t" code_of(t) "\t" $0
      next
    }
    {
      if (inblock) { if (index(t, blockend)) inblock = 0; next }
      if (lang == "js" && t ~ /^\/\*/) { if (!index(substr(t, 3), "*/")) { inblock = 1; blockend = "*/" } next }
      if (lang == "py" && (t ~ /^"""/ || t ~ /^\047\047\047/)) {
        q = substr(t, 1, 3); if (!index(substr(t, 4), q)) { inblock = 1; blockend = q } next
      }
      if (lang == "rb" && t ~ /^=begin/) { inblock = 1; blockend = "=end"; next }
      if (lang == "pl" && t ~ /^=[a-z]/) { inblock = 1; blockend = "=cut"; next }
      if (t == "") next
      if (lang != "js" && t ~ /^#/) next
      if (lang == "js" && t ~ /^\/\//) next
      msg = (t ~ /^(print|puts|warn|die|raise|throw|console\.(log|error|warn|info)|log(ger|ging)?\.[a-z]+|sys\.exit)[ (]/) ? 1 : 0
      code = t; gsub(/["\047`][ ]*,[ ]*["\047`]/, " ", code)
      print "script\t" NR "\t\t" msg "\t" code "\t" t "\t" $0
    }' "$1"
}

emit_lines() { # cid kind comp file dir  <- TSV on stdin
  jq -cR --arg cid "$1" --arg kind "$2" --arg comp "$3" --arg file "$4" --arg dir "$5" '
    split("\t") | {cid: $cid, kind: $kind, comp: $comp, file: $file, dir: $dir,
      ctx: .[0], line: (.[1] | tonumber), key: .[2], msg: (.[3] == "1"), text: .[4], literal: .[5], original: (.[6:] | join("\t"))}' >>"$WORK/lines.ndjson"
}

emit_comp() { # json object
  printf '%s\n' "$1" >>"$WORK/comps.ndjson"
}

manifest_paths() { # key -> plugin-relative paths the manifest declares for it
  [ -n "$MANIFEST" ] || return 0
  jq -r --arg k "$1" '.[$k] // empty | if type == "string" then . elif type == "array" then .[] | select(type == "string") else empty end' "$MANIFEST" 2>/dev/null \
    | sed 's#^\./##'
}

# Skills
{
  [ -d "$P/skills" ] && find "$P/skills" -mindepth 2 -maxdepth 2 -name SKILL.md -type f
  while IFS= read -r p; do
    a="$P/$p"
    if [ -f "$a" ]; then printf '%s\n' "$a"
    elif [ -f "$a/SKILL.md" ]; then printf '%s\n' "$a/SKILL.md"
    elif [ -d "$a" ]; then find "$a" -mindepth 2 -maxdepth 2 -name SKILL.md -type f
    fi
  done < <(manifest_paths skills)
} | sort -u >"$WORK/skills.txt"
while IFS= read -r f; do
  [ -n "$f" ] || continue
  d=$(dirname "$f"); short=$(fm_name "$f"); [ -n "$short" ] || short=$(basename "$d")
  cid="skill:$short"; comp="$NAME:$short"
  emit_comp "$(jq -nc --arg id "$cid" --arg name "$comp" --arg short "$short" --arg file "$(rel "$f")" --arg prel "$(prel "$f")" --arg dir "$(prel "$d")" \
    '{id: $id, kind: "skill", name: $name, short: $short, file: $file, prel: $prel, dir: $dir, line: 1}')"
  md_lines "$f" | emit_lines "$cid" skill "$comp" "$(rel "$f")" "$(prel "$d")"
done <"$WORK/skills.txt"

# Commands and agents
for kind in command agent; do
  dir=${kind}s
  {
    if [ -d "$P/$dir" ]; then
      if [ $kind = command ]; then find "$P/$dir" -name '*.md' -type f; else find "$P/$dir" -maxdepth 1 -name '*.md' -type f; fi
    fi
    while IFS= read -r p; do
      a="$P/$p"
      if [ -f "$a" ]; then printf '%s\n' "$a"; elif [ -d "$a" ]; then find "$a" -maxdepth 1 -name '*.md' -type f; fi
    done < <(manifest_paths "$dir")
  } | sort -u >"$WORK/$dir.txt"
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    short=$(basename "$f" .md)
    [ $kind = agent ] && { n=$(fm_name "$f"); [ -n "$n" ] && short=$n; }
    cid="$kind:$short"; comp="$NAME:$short"
    emit_comp "$(jq -nc --arg id "$cid" --arg kind "$kind" --arg name "$comp" --arg short "$short" --arg file "$(rel "$f")" --arg prel "$(prel "$f")" --arg dir "$(prel "$(dirname "$f")")" \
      '{id: $id, kind: $kind, name: $name, short: $short, file: $file, prel: $prel, dir: $dir, line: 1, evidence: "\($kind) \($name)"}')"
    md_lines "$f" | emit_lines "$cid" "$kind" "$comp" "$(rel "$f")" "$(prel "$(dirname "$f")")"
  done <"$WORK/$dir.txt"
done

# Hooks: hooks/hooks.json, files the manifest names, and hooks inline in it.
line_of() { # file needle [after-line] -> first line number containing needle, taken literally
  NEEDLE=$2 awk -v after="${3:-0}" 'NR > after && index($0, ENVIRON["NEEDLE"]) { print NR; exit }' "$1"
}
valid_json() { # a component file that does not parse is an error, not an absence
  jq empty "$1" >/dev/null 2>&1 || die "cannot read $(rel "$1"): not valid JSON"
}
hook_source() { # file jq-path-to-hooks-object
  local file=$1 path=$2 last=0 cid="hooks:$(rel "$1")"
  local entries
  valid_json "$file"
  # Every hook type counts as a hook: command, http, mcp_tool, prompt, agent. A
  # command hook's command and an http hook's URL are where it runs.
  entries=$(jq -c "$path"' | if type == "object" then (if (.hooks | type) == "object" then .hooks else . end) else empty end
    | to_entries[] | .key as $e | (.value | if type == "array" then .[] else empty end) | (.matcher // "") as $m
    | (.hooks // [])[] | select(type == "object") | (.type // "command") as $t
    | {event: $e, matcher: $m, type: $t,
       runs: (if $t == "command" then (.command // "") elif $t == "http" then (.url // "") else "" end),
       what: (.command // .url // .prompt // .tool // .agent // "" | tostring)}' "$file" 2>/dev/null) || return 0
  [ -n "$entries" ] || return 0
  emit_comp "$(jq -nc --arg id "$cid" --arg file "$(rel "$file")" --arg prel "$(prel "$file")" \
    '{id: $id, kind: "hook", name: "hooks", short: "hooks", file: $file, prel: $prel, dir: "", line: 1}')"
  while IFS= read -r e; do
    local runs needle ln
    runs=$(jq -r .runs <<<"$e")
    needle=$(jq -r '(if .what != "" then .what else "\"type\": \"\(.type)\"" end) | tojson | .[1:-1] | .[0:60]' <<<"$e")
    ln=$(line_of "$file" "$needle" "$last"); [ -n "$ln" ] || ln=$(line_of "$file" "$needle" 0); [ -n "$ln" ] || ln=1
    last=$ln
    jq -c --arg cid "$cid" --arg file "$(rel "$file")" --argjson line "$ln" '. + {cid: $cid, comp: "hooks", kind: "hook", file: $file, line: $line}' <<<"$e" >>"$WORK/hooks.ndjson"
    if [ -n "$runs" ]; then
      c1=$(printf '%s' "$runs" | tr '\t\n' '  ')
      printf 'hook\t%s\t\t0\t%s\t%s\t%s\n' "$ln" "$c1" "$c1" "$c1" | emit_lines "$cid" hook hooks "$(rel "$file")" ""
    fi
  done <<<"$entries"
}
[ -f "$P/hooks/hooks.json" ] && hook_source "$P/hooks/hooks.json" '.'
while IFS= read -r p; do [ -f "$P/$p" ] && [ "$P/$p" != "$P/hooks/hooks.json" ] && hook_source "$P/$p" '.'; done < <(manifest_paths hooks)
[ -n "$MANIFEST" ] && [ "$(jq -r '.hooks | type' "$MANIFEST")" = object ] && hook_source "$MANIFEST" '.hooks'

# MCP and LSP servers: .mcp.json / .lsp.json, files the manifest names, inline.
server_source() { # kind file jq-path
  local kind=$1 file=$2 path=$3
  valid_json "$file"
  jq -c "$path"' | if type == "object" then (if (.mcpServers | type) == "object" then .mcpServers else . end) else empty end
    | to_entries[] | select(.value | type == "object") | {server: .key,
      transport: (.value.type // (if .value.url then "http" else "stdio" end)),
      has_headers: (((.value.headers // {}) | length > 0) or (.value.headersHelper != null))}' "$file" 2>/dev/null \
  | while IFS= read -r s; do
      local server ln
      server=$(jq -r .server <<<"$s")
      ln=$(line_of "$file" "\"$server\""); [ -n "$ln" ] || ln=1
      emit_comp "$(jq -c --arg kind "$kind" --arg file "$(rel "$file")" --arg prel "$(prel "$file")" --argjson line "$ln" \
        '{id: "\($kind):\(.server)", kind: $kind, name: "\($kind):\(.server)", short: .server, file: $file, prel: $prel, dir: "", line: $line,
          transport, has_headers, evidence: "\($kind) server \(.server) (\(.transport)\(if .has_headers then ", with headers" else "" end))"}' <<<"$s")"
    done
}
for kind in mcp lsp; do
  key=$([ $kind = mcp ] && echo mcpServers || echo lspServers)
  [ -f "$P/.$kind.json" ] && server_source "$kind" "$P/.$kind.json" '.'
  while IFS= read -r p; do [ -f "$P/$p" ] && [ "$P/$p" != "$P/.$kind.json" ] && server_source "$kind" "$P/$p" '.'; done < <(manifest_paths "$key")
  [ -n "$MANIFEST" ] && [ "$(jq -r --arg k "$key" '.[$k] | type' "$MANIFEST")" = object ] && server_source "$kind" "$MANIFEST" ".$key"
done

# Scripts: anything executable-looking outside assets/, tests and test data.
find "$P" \( -name .git -o -name node_modules -o -name testdata -o -name test -o -name tests -o -name .claude-plugin \) -prune -o -type f -print \
  | grep -v "^$P/assets/" | sort >"$WORK/files.txt"
while IFS= read -r f; do
  b=$(basename "$f")
  case $b in *.md|*.json|*_test.*|*.test.*|test_*) continue ;; esac
  case $b in
    *.sh|*.bash|*.zsh|*.py|*.js|*.mjs|*.cjs|*.ts|*.rb|*.pl) ;;
    *) head -c 2 "$f" 2>/dev/null | grep -q '^#!' || continue ;;
  esac
  r=$(prel "$f"); cid="script:$r"
  emit_comp "$(jq -nc --arg id "$cid" --arg name "$r" --arg file "$(rel "$f")" --arg base "$b" --arg dir "$(prel "$(dirname "$f")")" \
    '{id: $id, kind: "script", name: $name, short: $base, rel: $name, prel: $name, base: $base, file: $file, dir: $dir, line: 1}')"
  script_lines "$f" | emit_lines "$cid" script "$r" "$(rel "$f")" "$(prel "$(dirname "$f")")"
done <"$WORK/files.txt"

jq -s '.' "$WORK/comps.ndjson" >"$WORK/comps.json"
jq -s '.' "$WORK/lines.ndjson" >"$WORK/lines.json"
jq -s '.' "$WORK/hooks.ndjson" >"$WORK/hooks.json"

# --- what the repository delivers ---------------------------------------------------

facts='{"known": false}'
if [ -n "$REPO" ]; then
  declared='{}'
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    f="$REPO/$path"
    if [ ! -f "$f" ]; then
      declared=$(jq -c --arg p "$path" '. + {($p): {exists: false, declared: false}}' <<<"$declared")
      continue
    fi
    key=$(jq -r --arg n "$NAME" '[.enabledPlugins // {} | to_entries[] | select(.value == true and (.key | startswith($n + "@")))][0].key // ""' "$f" 2>/dev/null)
    ln=null
    [ -n "$key" ] && { ln=$(line_of "$f" "\"$key\""); [ -n "$ln" ] || ln=null; }
    declared=$(jq -c --arg p "$path" --arg key "$key" --argjson line "$ln" \
      '. + {($p): {exists: true, declared: ($key != ""), key: $key, line: $line}}' <<<"$declared")
  done < <(jq -r '.. | objects | select(has("declared_in")) | .declared_in' "$WORK/profiles.json" | sort -u)

  # Two ways a repository can commit a plugin where a target loads it: the whole
  # plugin under one of the layout's plugin_dirs (its manifest and every component
  # file), or each skill, agent and command where the layout says that kind loads.
  # The second delivers only those three kinds, which the vendored rules report.
  vendored='{}'
  jq -r '[.[] | .prel // empty] | unique[]' "$WORK/comps.json" >"$WORK/prels.txt"
  while IFS= read -r t; do
    profile=$(jq -c --arg t "$t" '.[] | select(.id == $t)' "$WORK/profiles.json")
    via=null missing=()
    while IFS= read -r pat; do
      [ -n "$pat" ] || continue
      d="$REPO/${pat//\{plugin\}/$NAME}"
      [ -d "$d" ] || continue
      [ -z "$MANIFEST" ] || [ -f "$d/.claude-plugin/plugin.json" ] || continue
      whole=true
      while IFS= read -r r; do [ -e "$d/$r" ] || { whole=false; break; }; done <"$WORK/prels.txt"
      $whole && [ -s "$WORK/prels.txt" ] && { via='"plugin_dir"'; break; }
    done < <(jq -r '.layout.plugin_dirs[]?' <<<"$profile")
    total=0
    while IFS=$'\t' read -r kind short; do
      [ -n "$kind" ] || continue
      total=$((total + 1)); found=false
      while IFS= read -r pat; do
        [ -n "$pat" ] && [ -e "$REPO/${pat//\{name\}/$short}" ] && found=true
      done < <(jq -r --arg k "${kind}s" '.layout[$k][]?' <<<"$profile")
      $found || missing+=("$kind $short")
    done < <(jq -r '.[] | select(.kind == "skill" or .kind == "agent" or .kind == "command") | "\(.kind)\t\(.short)"' "$WORK/comps.json")
    if [ "$via" = null ] && [ "$total" -gt 0 ] && [ ${#missing[@]} -eq 0 ]; then via='"components"'; fi
    vendored=$(jq -c --arg t "$t" --argjson via "$via" --argjson total "$total" \
      --argjson missing "$(printf '%s\n' "${missing[@]+"${missing[@]}"}" | jq -R . | jq -sc 'map(select(length > 0))')" \
      '. + {($t): {vendored: ($via != null), via: $via, components: $total, missing: $missing}}' <<<"$vendored")
  done < <(jq -r '.[].id' "$WORK/profiles.json")
  facts=$(jq -nc --arg root "$REPO" --argjson declared "$declared" --argjson vendored "$vendored" \
    '{known: true, root: $root, declared: $declared, vendored: $vendored}')
fi
printf '%s\n' "$facts" >"$WORK/facts.json"

# --- review findings ----------------------------------------------------------------

if [ -n "$REVIEW" ]; then
  [ -f "$REVIEW" ] || die "--review $REVIEW: no such file"
  jq -e 'type == "array"' "$REVIEW" >/dev/null 2>&1 || die "--review $REVIEW: not a JSON array of findings"
  cp "$REVIEW" "$WORK/review.json"
else
  echo '[]' >"$WORK/review.json"
fi

# --- apply the rules ------------------------------------------------------------------

read -r -d '' SCAN <<'JQ'
def has_item($x): any(.[]; . == $x);
def strip: sub("^\\s+"; "") | sub("\\s+$"; "");
def rank: {"breaks": 4, "needs-setup": 3, "degrades": 2, "info": 1, "works": 0}[.] // 0;
def sev_of_rank: ["works", "info", "degrades", "needs-setup", "breaks"][.];
def worst: (map(rank) | max // 0) | sev_of_rank;
def rx: gsub("(?<c>[.*+?^${}()|\\[\\]\\\\/])"; "\\\(.c)");

$profiles[0] as $profiles | $comps[0] as $comps | $lines[0] as $lines | $hooks[0] as $hooks
| $facts[0] as $facts | $review[0] as $review
| ($comps | map({(.id): .}) | add // {}) as $byid

# Tool names a frontmatter line declares.
| ([ $lines[] | select(.ctx == "frontmatter" and (.key == "allowed-tools" or .key == "tools")) | . as $l
    | .text
    | (if test("^\\s*-\\s") then sub("^\\s*-\\s*"; "") else sub("^[^:]*:\\s*"; "") end)
    | gsub("\\([^)]*\\)"; "") | gsub("[\\[\\]\"']"; "")
    | [splits("[,\\s]+")] | map(select(length > 0) | capture("^(?<n>[A-Za-z0-9_*/.:-]+)").n)
    | .[] | {cid: $l.cid, comp: $l.comp, kind: $l.kind, file: $l.file, line: $l.line, field: $l.key, tool: ., evidence: "\($l.key): \(.)"} ]
  # A fenced Agent(subagent_type: …) spawn uses the Agent tool whether or not the
  # frontmatter declares it: allowed-tools only pre-approves.
  + [ $lines[] | select(.ctx == "fence" and (.msg | not) and (.original | test("subagent_type\\s*[:=]")))
      | {cid, comp, kind, file, line, field: "spawn", tool: "Agent", evidence: "spawn: \(.original | strip)"} ])
  as $tools

# Edges: who runs, spawns or invokes whom, where it runs.
| [ $comps[] | select(.kind == "script") ] as $scripts
| [ $comps[] | select(.kind == "agent") ] as $agents
| [ $comps[] | select(.kind == "skill") ] as $skills
| [ $lines[] | select(.msg | not) | . as $l
    | ( $scripts[] | select(.id != $l.cid) | . as $s
        | select(
            if $l.ctx == "script" then ($l.text | test("(^|[^A-Za-z0-9_.-])" + ($s.base | rx) + "($|[^A-Za-z0-9_.-])"))
            elif $l.ctx == "fence" or $l.ctx == "hook" then
              ($l.original | contains($s.rel))
              or ($l.dir != "" and ($s.rel | startswith($l.dir + "/")) and ($l.original | contains($s.rel[($l.dir | length) + 1:])))
            else false end)
        | {from: $l.cid, to: $s.id, file: $l.file, line: $l.line} ),
      ( if $l.ctx == "fence" then
          ( $agents[] | select(.id != $l.cid) | . as $a
            | select($l.original | test("subagent_type\\s*[:=]\\s*[\"']?(" + ($a.name | split(":")[0] | rx) + ":)?" + ($a.short | rx) + "(?![A-Za-z0-9_-])"))
            | {from: $l.cid, to: $a.id, file: $l.file, line: $l.line} ),
          ( $skills[] | select(.id != $l.cid) | . as $s
            | select($l.original | test("(?<![A-Za-z0-9_-])" + ($s.name | rx) + "(?![A-Za-z0-9_-])"))
            | {from: $l.cid, to: $s.id, file: $l.file, line: $l.line} )
        elif $l.ctx == "frontmatter" and $l.key == "skills" then
          ( ($l.text | (if test("^\\s*-\\s") then sub("^\\s*-\\s*"; "") else sub("^[^:]*:\\s*"; "") end)
              | [splits("[,\\s\\[\\]\"']+")] | map(select(length > 0))) as $toks
            | $skills[] | . as $s
            | select($toks | any(. == $s.short or . == $s.name))
            | {from: $l.cid, to: $s.id, file: $l.file, line: $l.line} )
        else empty end )
  ] | unique_by([.from, .to]) as $edges

# Everything a component reaches, with the path to it.
| def reach($start):
    {seen: {($start): []}, frontier: [$start]}
    | until(.frontier | length == 0;
        . as $s
        | [ $s.frontier[] as $f | $edges[] | select(.from == $f and $s.seen[.to] == null) | {to, via: ($s.seen[$f] + [.to])} ]
        | unique_by(.to) as $new
        | $s | .seen += ($new | map({(.to): .via}) | add // {}) | .frontier = ($new | map(.to)))
    | .seen | to_entries | map(select(.key != $start) | {id: .key, via: .value});

  def finding($p; $r; $f):
    { target: $p.id, rule: $r.id, severity: ($f.severity // $r.severity),
      component: $f.component, component_kind: $f.kind, file: $f.file, line: $f.line,
      context: ($f.context // null), evidence: $f.evidence,
      fix_in: $r.fix_in, remedy: $r.remedy, rule_remedy: $r.remedy, sources: $r.sources, basis: $r.basis,
      unconfirmed: ($r.basis == "unconfirmed"), checked: $r.checked, summary: $r.summary }
    + (if $r.observed then {observed: $r.observed} else {} end)
    + (if $f.guard then {guard: ($f.guard | {line, evidence})} else {} end);

  # The lines of code that guard a script against a rule: each script line whose
  # literal — its code, quoted text kept — matches a command rule's guard pattern.
  # Comments, message commands and heredoc text never reach a line's literal.
  def guards_of($p):
    [ $p.rules[] | select(.match.kind == "command" and (.guard | type) == "object") | . as $r
      | $lines[] | select(.ctx == "script" and (.msg | not) and (.literal | test($r.guard.pattern)))
      | {rule: $r.id, severity: $r.guard.severity, component: .comp, file: .file, line: .line, evidence: (.original | strip)} ];

  # The guard, if any, that rates a rule's match on this line of this script:
  # another line of the same script, the first in it.
  def guard_for($guards; $rule; $script; $line):
    [ $guards[] | select(.rule == $rule and .component == $script and .line != $line) ][0];

  def host_ok($allow): . as $h | any($allow[]; . as $a | if ($a | startswith("*.")) then ($h | endswith($a[1:])) else $h == $a end);
  def host_ignored:
    test("^(localhost|0\\.0\\.0\\.0)$") or test("^[0-9.]+$")
    or test("(^|\\.)(example\\.(com|org|net)|example|test|invalid|local|localhost|internal)$");

  def evalwhen($w; $tid):
    if $w | has("declared_in") then ($facts.declared[$w["declared_in"]].declared // false)
    elif $w | has("vendored") then
      (if $w["vendored"] == true then ($facts.vendored[$tid].vendored // false)
       else ($facts.vendored[$tid].via // null) == $w["vendored"] end)
    elif $w | has("not") then (evalwhen($w["not"]; $tid) | not)
    elif $w | has("all") then all($w["all"][]; evalwhen(.; $tid))
    elif $w | has("any") then any($w["any"][]; evalwhen(.; $tid))
    else false end;

  def delivery_evidence($p; $w):
    [ $w | .. | objects | select(has("declared_in")) | .declared_in ] as $paths
    | ([ $w | .. | objects | select(has("vendored")) ] | length > 0) as $usesv
    | $facts.vendored[$p.id] as $v
    | ( [ $paths[] | . as $path | $facts.declared[$path] as $d
          | if $d.declared then "\($path):\($d.line // "?") enables \($d.key)"
            elif $d.exists then "\($path) does not enable \($NAME)"
            else "\($path) does not exist" end ]
        + (if $usesv then
             [ if $v.via == "plugin_dir" then "the whole plugin is committed where \($p.id) loads repository plugins"
               elif $v.via == "components" then "\($NAME)'s skills, agents and commands are committed where \($p.id) loads them"
               elif $v.components == 0 then "\($NAME) has no skill, agent or command to commit"
               else "not committed where \($p.id) loads components: \($v.missing | join(", ")) missing" end ]
           else [] end) ) | join("; ")
    | {evidence: .,
       file: (if ($paths | length) > 0 then $paths[0] else "\($PLUGIN_FILE)" end),
       line: (if ($paths | length) > 0 then $facts.declared[$paths[0]].line else 1 end)};

  def match_rule($p; $r; $guards):
    $r.match as $m
    | if $m.kind == "command" then
        $lines[] | select((.msg | not) and (.ctx as $c | $m["in"] | has_item($c))) | select(.text | test($m.pattern))
        | (if .ctx == "script" then guard_for($guards; $r.id; .comp; .line) else null end) as $g
        | finding($p; $r; {severity: (if $g then $g.severity else ($r.severity_in[.ctx] // $r.severity) end),
                           component: .comp, kind: .kind, file: .file, line: .line, context: .ctx, evidence: (.original | strip), guard: $g})
      elif $m.kind == "url" then
        $lines[] | select((.msg | not) and (.ctx as $c | $m["in"] | has_item($c))) | . as $l
        | [ .text | scan("https?://([A-Za-z0-9][A-Za-z0-9.-]*[A-Za-z0-9])") | .[0] | ascii_downcase ] | unique[]
        | select(contains(".") and (host_ignored | not) and (host_ok($m.allow) | not))
        | finding($p; $r; {severity: ($r.severity_in[$l.ctx] // $r.severity), component: $l.comp, kind: $l.kind, file: $l.file, line: $l.line, context: $l.ctx, evidence: "\(.): \($l.original | strip)"})
      elif $m.kind == "env" then
        # A credential read from the environment: $NAME, ${NAME…}, ${!NAME}, a bare
        # NAME on the right of an assignment (a variable that holds the name), an
        # `export NAME=` in a fenced block (an instruction to set it), and the
        # environment lookups of other languages. A name the same file assigns
        # itself is a local variable, not the environment. A name tested only for
        # presence — [ -n "${NAME:-}" ], [[ -z $NAME ]], test -n "$NAME" — is
        # not read either: the line asks whether the credential is set, to
        # explain a misconfiguration, and never uses its value.
        ( [ $lines[] | select(.ctx == "script" or .ctx == "fence") | .file as $f | .ctx as $c | .text
            | capture(if $c == "script" then "^\\s*(?:export\\s+|local\\s+|readonly\\s+|typeset\\s+|declare(?:\\s+-\\w+)*\\s+)?(?<n>[A-Za-z_][A-Za-z0-9_]*)="
                      else "^\\s*(?:local\\s+|readonly\\s+|typeset\\s+|declare(?:\\s+-\\w+)*\\s+)?(?<n>[A-Za-z_][A-Za-z0-9_]*)=" end).n // empty
            | {file: $f, name: .} ] | group_by(.file) | map({(.[0].file): map(.name)}) | add // {} ) as $assigned
        | $lines[] | select((.msg | not) and (.ctx as $c | $m["in"] | has_item($c))) | . as $l
        | (.text | gsub("(?:\\[\\[?|(?<![A-Za-z0-9_.-])test)\\s+-[nz]\\s+\"?\\$\\{?[A-Za-z_][A-Za-z0-9_]*(?::?[-=+?][^}\"\\s]*)?\\}?\"?(?:\\s+\\]\\]?)?"; " ")) as $text
        | ([ ($text | scan("\\$\\{?!?([A-Z][A-Z0-9_]*)") | .[0]),
             ($text | scan("=([A-Z][A-Z0-9_]*)(?![A-Za-z0-9_])") | .[0]),
             (if .ctx == "fence" then ($text | scan("^\\s*export\\s+([A-Z][A-Z0-9_]*)=") | .[0]) else empty end),
             ($text | scan("(?:environ(?:\\.get)?[\\[(]|getenv\\(|ENV\\[|ENV\\.fetch\\()[\"']([A-Z][A-Z0-9_]*)") | .[0]),
             ($text | scan("process\\.env\\.([A-Z][A-Z0-9_]*)") | .[0]) ] | unique) as $names
        | $names[] | . as $n | select(($n | test($m.name_pattern)) and ((($assigned[$l.file] // []) | has_item($n)) | not))
        | finding($p; $r; {severity: ($r.severity_in[$l.ctx] // $r.severity), component: $l.comp, kind: $l.kind, file: $l.file, line: $l.line, context: $l.ctx, evidence: "\(.): \($l.original | strip)"})
      elif $m.kind == "vendored" then
        # Only when the repository delivers the plugin by committing its skills,
        # agents and commands: what that route does not carry.
        if $facts.known and (($facts.vendored[$p.id].via // null) == ($m.via // "components")) then
          ( if $m.types then
              $comps[] | select(.kind as $k | $m.types | has_item($k))
              | finding($p; $r; {component: .name, kind: .kind, file: .file, line: .line, evidence: "\(.kind) \(.name) is not carried by the committed skills"})
            else empty end ),
          ( if $m.pattern then
              $lines[] | select((.msg | not) and (.kind == "skill" or .kind == "agent" or .kind == "command") and (.ctx as $c | ($m["in"] // ["fence"]) | has_item($c)))
              | select(.text | test($m.pattern))
              | finding($p; $r; {component: .comp, kind: .kind, file: .file, line: .line, context: .ctx, evidence: (.original | strip)})
            else empty end )
        else empty end
      elif $m.kind == "tool" then
        $tools[] | select((.kind as $k | $m.components | has_item($k)) and (.field as $f | $m.fields | has_item($f)))
        # Tool names compare case-insensitively: Copilot's tool aliases are.
        | select(.tool as $t | ($t | ascii_downcase) as $lt |
            if $m.not_in then
              ($m.not_in | map(ascii_downcase) | has_item($lt) | not) and (($m.ignore // []) | map(ascii_downcase) | has_item($lt) | not)
              and (if $m.ignore_pattern then ($t | test($m.ignore_pattern; "i") | not) else true end)
            else
              (($m.names // []) | map(ascii_downcase) | has_item($lt)) or (if $m.name_pattern then ($t | test($m.name_pattern; "i")) else false end)
            end)
        # A declaration is frontmatter, a spawn is a fence: severity_in can tell a
        # component that may use the tool from one whose flow runs it.
        | (if .field == "spawn" then "fence" else "frontmatter" end) as $ctx
        | finding($p; $r; {severity: ($r.severity_in[$ctx] // $r.severity), component: .comp, kind: .kind, file: .file, line: .line, context: $ctx, evidence: .evidence})
      elif $m.kind == "component" then
        if $m.type == "hook" then
          $hooks[] | finding($p; $r; {component: .comp, kind: "hook", file: .file, line: .line, context: "hook",
            evidence: "\(.event)\(if .matcher != "" then "[\(.matcher)]" else "" end): \(.type) \(.what)"})
        else
          $comps[] | select(.kind == $m.type) | . as $c
          | select(($m.where.transport == null or ($m.where.transport | has_item($c.transport)))
                   and ($m.where.has_headers == null or $m.where.has_headers == $c.has_headers))
          | finding($p; $r; {component: .name, kind: .kind, file: .file, line: .line, evidence: .evidence})
        end
      elif $m.kind == "delivery" then
        if $facts.known and evalwhen($m.when; $p.id) then
          delivery_evidence($p; $m.when) as $e
          | finding($p; $r; {component: "delivery", kind: "delivery", file: $e.file, line: $e.line, evidence: $e.evidence})
        else empty end
      else empty end;

  def drop_excepted($p):
    . as $fs
    | [ $fs[] | . as $f
        | ([ $p.rules[] | select(.id == $f.rule) | .match.except // [] ] | add // []) as $ex
        | select(any($fs[]; . as $g | ($ex | has_item($g.rule)) and $g.component == $f.component and $g.file == $f.file and $g.line == $f.line
                                 and (($g.severity | rank) >= ($f.severity | rank))) | not) ];

  # A review finding on a script that carries its rule's guard — a call the scan
  # cannot see, such as one made through the script's own shell function — takes
  # the guard's severity unless it gives its own, as a scanned match would.
  def review_for($p; $guards):
    [ $review[] | select(.target == $p.id) | . as $f
      | ($p.rules | map(select(.id == $f.rule)) | .[0]) as $r
      | (if $f.severity == null then guard_for($guards; $f.rule; $f.component; $f.line) else null end) as $g
      | { target: $p.id, rule: $f.rule, severity: ($f.severity // $g.severity // $r.severity),
          component: $f.component, component_kind: ($f.component_kind // (if $f.component == "delivery" then "delivery" else ([ $comps[] | select(.name == $f.component) | .kind ][0]) end)),
          file: $f.file, line: $f.line, context: "review", evidence: $f.evidence,
          fix_in: ($f.fix_in // $r.fix_in), remedy: ($f.remedy // $r.remedy), rule_remedy: $r.remedy, sources: ($f.sources // $r.sources),
          basis: "review", unconfirmed: ($f.unconfirmed // ($r.basis == "unconfirmed")), checked: $r.checked, summary: $r.summary }
        + (if $f.via then {via: $f.via} else {} end)
        + (if $g then {guard: ($g | {line, evidence})} else {} end) ];

  def summarize: # findings -> per-rule entries
    group_by(.rule) | map(
      (map(.severity) | worst) as $w
      | { rule: .[0].rule, severity: $w,
          unconfirmed: all(.[] | select(.severity == $w); .unconfirmed),
          review: (if all(.[]; .basis == "review") then "all" elif any(.[]; .basis == "review") then "some" else null end),
          own: (map(select(.inherited | not)) | length),
          via: (map(select((.via // []) | length > 0) | (if .basis == "review" then "(review) " else "" end) + (.via | join(" → "))) | unique) })
    | sort_by(-(.severity | rank), .rule);

# Review findings must cite a rule of the target's profile and stay well-formed.
  [ $review[] | . as $f
    | ($profiles | map(select(.id == $f.target)) | .[0]) as $p
    | if (type != "object") then "a review finding is not an object"
      elif $p == null then "review finding names target \($f.target // "?"), which is not scanned"
      elif ($p.rules | map(.id) | has_item($f.rule) | not) then "review finding cites rule \($f.rule // "?"), which \($p.id) does not define"
      elif ($f.severity != null and (["breaks", "needs-setup", "degrades", "info"] | has_item($f.severity) | not)) then "review finding \($f.rule) has a severity that is not breaks, needs-setup, degrades or info"
      elif ($f.basis != null and $f.basis != "review") then "review finding \($f.rule) claims basis \($f.basis); review findings are basis review"
      elif (($f.component | type) != "string") or ($f.component != "delivery" and ([ $comps[] | .name ] | has_item($f.component) | not)) then "review finding \($f.rule) names component \($f.component // "?"), which the plugin does not have"
      elif (($f.file | type) != "string") or (($f.evidence | type) != "string") then "review finding \($f.rule) needs file and evidence"
      elif ($f.line != null and ($f.line | type) != "number") then "review finding \($f.rule) has a line that is not a number"
      elif ($f.fix_in != null and (["plugin", "repository", "environment"] | has_item($f.fix_in) | not)) then "review finding \($f.rule) has a fix_in that is not plugin, repository or environment"
      elif ($f.sources != null and ((($f.sources | type) != "array") or ($f.sources | length) == 0 or any($f.sources[]; (type != "string") or (test("^https://") | not)))) then "review finding \($f.rule) has sources that are not a non-empty list of https URLs"
      elif ($f.component_kind != null and ($f.component_kind != (if $f.component == "delivery" then "delivery" else ([ $comps[] | select(.name == $f.component) | .kind ] | if has_item($f.component_kind) then $f.component_kind else null end) end))) then "review finding \($f.rule) gives component_kind \($f.component_kind), which \($f.component) is not"
      elif ($f.via != null and ((($f.via | type) != "array") or any($f.via[]; type != "string"))) then "review finding \($f.rule) has a via that is not a list of component names"
      elif ($f.remedy != null and ($f.remedy | type) != "string") then "review finding \($f.rule) has a remedy that is not text"
      else empty end ] as $review_errors
| if ($review_errors | length) > 0 then {error: ($review_errors | unique | join("; "))} else

  [ $profiles[] | . as $p
    | guards_of($p) as $guards
    | ([ $p.rules[] | . as $r | match_rule($p; $r; $guards) ] | drop_excepted($p)) as $scanned
    | ($scanned + review_for($p; $guards)) as $fs
    | [ $fs[] | select(.component == "delivery") ] as $dfs
    | ([ $dfs[] | .severity ] | worst) as $dv
    | { assessed: $facts.known,
        verdict: (if $facts.known then (if ($dv | rank) <= 1 then "works" else $dv end) else "not assessed" end),
        unconfirmed: ($facts.known and ($dv | rank) > 1 and all($dfs[] | select(.severity == $dv); .unconfirmed)),
        rules: ($dfs | summarize) } as $delivery
    | [ $comps[] | . as $c
        | [ $fs[] | select(.component == $c.name and .component_kind == $c.kind) ] as $own
        | [ reach($c.id)[] | . as $hop
            | ($hop.via | map($byid[.].name)) as $path
            | $fs[] | select(.component == $byid[$hop.id].name and .component_kind == $byid[$hop.id].kind)
            | . as $f | $f + {inherited: true, via: ($path + (if $f.basis == "review" then ($f.via // []) else [] end))} ] as $inherited
        | (if $delivery.verdict == "breaks" then
             [ $dfs[] | select(.severity == "breaks") | . + {via: ["delivery"]} ]
           else [] end) as $carried
        | ($own + $inherited + $carried) as $all
        | ([ $all[] | .severity ] | worst) as $w
        | (if ($w | rank) <= 1 then "works" else $w end) as $verdict
        | { component: $c.name, kind: $c.kind, file: $c.file, verdict: $verdict,
            unconfirmed: ($verdict != "works" and all($all[] | select(.severity == $w); .unconfirmed)),
            set_by: (if $verdict == "works" then [] else [ $all[] | select(.severity == $w) | .rule ] | unique end),
            rules: ($all | summarize) } ]
      as $components
    | { id: $p.id, name: $p.name, profile: $p._file, checked: $p.checked,
        delivery: $delivery,
        components: $components,
        findings: ($fs | sort_by(-(.severity | rank), .rule, .file, (.line // 0))),
        guards: $guards,
        review_rules: [ $p.rules[] | select(.match.kind == "review") | {id, severity, basis, summary, look_for: .match.look_for, remedy, fix_in, sources} ] } ]
  | . as $targets
  | { plugin: {name: $NAME, version: $VERSION, dir: $PLUGIN_DIR, manifest: $PLUGIN_FILE},
      repository: (if $facts.known then $facts.root else null end),
      components: [ $comps[] | {name, kind, file} ],
      edges: [ $edges[] | {from: $byid[.from].name, to: $byid[.to].name, file, line} ],
      targets: $targets,
      summary: ([ $targets[].findings[] | .severity ] | group_by(.) | map({(.[0]): length}) | add // {}) }
  end
JQ

result=$(jq -n \
  --slurpfile profiles "$WORK/profiles.json" --slurpfile comps "$WORK/comps.json" \
  --slurpfile lines "$WORK/lines.json" --slurpfile hooks "$WORK/hooks.json" \
  --slurpfile facts "$WORK/facts.json" --slurpfile review "$WORK/review.json" \
  --arg NAME "$NAME" --arg VERSION "$VERSION" \
  --arg PLUGIN_DIR "$(if [ "$BASE" = "$REPO" ] && [ -n "$REPO" ]; then rel "$P"; else printf '%s' "$P"; fi)" \
  --arg PLUGIN_FILE "$(if [ -n "$MANIFEST" ]; then rel "$MANIFEST"; else rel "$P"; fi)" \
  "$SCAN" 2>"$WORK/err") || die "scan failed: $(cat "$WORK/err")"
err=$(jq -r '.error // empty' <<<"$result")
[ -z "$err" ] || die "--review $REVIEW: $err"

# --- report -----------------------------------------------------------------------------

read -r -d '' MARKDOWN <<'JQ'
def rank: {"breaks": 4, "needs-setup": 3, "degrades": 2, "info": 1, "works": 0}[.] // 0;
def code: tostring | if test("`") then "`` " + . + " ``" else "`" + . + "`" end;
def cell: tostring | gsub("\\|"; "\\|") | gsub("\n"; " ");
def loc: if .line then "\(.file):\(.line)" else "\(.file) (absent)" end;
def clip: if length > 120 then .[0:117] + "…" else . end;
def verdict_text: (if .verdict == "works" or .verdict == "not assessed" then .verdict else "**\(.verdict)**" end)
  + (if .unconfirmed then ", unconfirmed" else "" end);
def entry_text: "\(.rule | code) \(.severity)\(if .unconfirmed then " (unconfirmed)" else "" end)\(if .review == "all" then " (review)" elif .review == "some" then " (scan and review)" else "" end)"
  + (if (.via | length) > 0 then " via " + ((.via[0:3] | join("; ")) + (if (.via | length) > 3 then "; +\((.via | length) - 3) more" else "" end)) else "" end);
def why: [ .rules[] | select((.severity | rank) > 1) | entry_text ] | join("<br>") | cell;

. as $r
| "# Hosted-agent review: \($r.plugin.name | code)\n\n"
+ "Plugin \($r.plugin.name | code)\(if $r.plugin.version != "" then " " + $r.plugin.version else "" end) at \($r.plugin.dir | code)"
+ (if $r.repository then ", delivered from the repository at \($r.repository | code)." else ". No repository: delivery was not assessed." end)
+ "\n\nVerdicts: **breaks** fails there; **needs-setup** works once the repository or environment is configured, with no change to the plugin; **degrades** works partly; *works* has nothing worse than info. A verdict marked unconfirmed rests on a rule the sources leave open. Findings marked (review) come from reading the plugin's prose, not from the scan.\n\n"
+ "| Component | " + ([ $r.targets[] | .id | code ] | join(" | ")) + " |\n"
+ "|---|" + ([ $r.targets[] | "---" ] | join("|")) + "|\n"
+ "| Delivery | " + ([ $r.targets[] | .delivery | verdict_text ] | join(" | ")) + " |\n"
+ ([ $r.components[] | . as $c
     | "| \($c.name | code) (\($c.kind)) | " + ([ $r.targets[] | .components[] | select(.component == $c.name and .kind == $c.kind) | verdict_text ] | join(" | ")) + " |\n" ] | join(""))
+ ([ $r.targets[] | . as $t
    | "\n## \($t.name) — \($t.id | code)\n\n"
    + "Profile \($t.profile | split("/") | .[-3:] | join("/") | code), checked **\($t.checked)**. Rules carry their own checked dates below; a date long past means the facts may have moved.\n\n"
    + (if $t.delivery.verdict == "breaks" then
         "**The plugin cannot be delivered to \($t.id | code), so every component below breaks.**\n\n" else "" end)
    + (if $t.delivery.assessed then
         "**Delivery:** \($t.delivery | verdict_text) — " + ([ $t.findings[] | select(.component == "delivery") | "\(.rule | code): \(.evidence)" ] | join("; ")) + "\n\n"
       else "**Delivery: not assessed** — no repository was given.\n\n" end)
    + "| Component | Verdict | Why |\n|---|---|---|\n"
    + ([ $t.components[] | "| \(.component | code) (\(.kind)) | \(verdict_text) | \(why) |\n" ] | join(""))
    + (if ($t.findings | length) == 0 then "\nNo findings.\n" else
        "\n### Findings\n"
        + ([ $t.findings | group_by(.rule) | sort_by(-(map(.severity | rank) | max), .[0].rule)[] | . as $g | $g[0] as $f
             | "\n#### \($f.rule | code) — \([ $g[].severity ] | unique | sort_by(-rank) | join(", "))\(if $f.unconfirmed then ", unconfirmed" else "" end)\n\n"
             + "\($f.summary)\n\n"
             + "- **Remedy** (fix in \($f.fix_in)): \($f.rule_remedy)\n"
             + "- **Basis:** \([ $g[].basis ] | unique | join(", ")), rule checked \($f.checked)"
             + (if $f.observed then "; observed: " + ([ $f.observed[] | "[\(.date)](\(.url)) \(.result)" ] | join(" ")) else "" end) + "\n"
             + "- **Sources:** \($f.sources | join(", "))\n"
             + "- **Where:**\n"
             + ([ $g[] | "  - \(loc | code) — \(.component | code)\(if .context and .context != "review" then " (\(.context))" else "" end), \(.severity)\(if .basis == "review" then " (review)" else "" end): \(.evidence | clip | code)"
                  + (if .guard then " — guarded at line \(.guard.line): \(.guard.evidence | clip | code)" else "" end)
                  + (if .remedy != .rule_remedy then " — \(.remedy)" else "" end)
                  + (if .basis == "review" and (.via // [] | length) > 0 then " — via \(.via | join(" → "))" else "" end) + "\n" ] | join(""))
           ] | join(""))
      end)
  ] | join(""))
JQ

if [ "$FORMAT" = markdown ]; then
  jq -r "$MARKDOWN" <<<"$result" || die "cannot render the report"
else
  printf '%s\n' "$result"
fi

if jq -e '[.targets[].findings[] | select(.severity == "breaks")] | length > 0' <<<"$result" >/dev/null; then
  exit 1
fi
exit 0
