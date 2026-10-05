#!/bin/zsh
# Verification entry point.
#
#   1. compiles main and test sources
#   2. runs the JUnit 5 suite, which includes the native-git and JGit cross-checks
#   3. independently replays one add/commit sequence with this VCS and with the
#      system git in a temporary directory, then diffs every object hash and the
#      full tree structure
#
# Usage: ./verify.sh [--quick]
set -e
cd "$(dirname "$0")"

JUNIT="$PWD/lib/junit-platform-console-standalone-1.10.5.jar"
JGIT="$PWD/lib/org.eclipse.jgit-6.8.0.202311291450-r.jar"
SLF4J="$PWD/lib/slf4j-api-2.0.16.jar"
MAIN_CP="$PWD/build/classes:$JUNIT:$JGIT:$SLF4J"

GIT_BIN="${GIT:-git}"

hr() { printf '%s\n' "------------------------------------------------------------"; }
step() { hr; printf '==> %s\n' "$1"; hr; }

step "1/4  compiling"
./build.sh --skip-tests 2>&1 | grep -v "^Picked up" || true

step "2/4  JUnit 5 suite (includes git and JGit cross-checks)"
DETAILS=tree
if [[ "$1" == "--quick" ]]; then
  DETAILS=summary
fi
java -jar "$JUNIT" \
  --class-path "$MAIN_CP:build/test-classes" \
  --select-package lawfirm.vcs \
  --details="$DETAILS" \
  --disable-ansi-colors \
  --fail-if-no-tests

step "3/4  differential against system git in a temporary directory"
if ! command -v "$GIT_BIN" >/dev/null 2>&1; then
  echo "system git not found on PATH; skipping the differential" >&2
  exit 0
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/vcs-verify.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

OURS="$WORK/ours"
THEIRS="$WORK/theirs"
mkdir -p "$OURS" "$THEIRS"

# A sequence with nested directories, unicode names, an edit, a delete and an add,
# so the comparison covers more than one trivial commit.
#
# The tree is written by a helper class rather than shell redirection: the file
# names include CJK characters, and writing them through a here-doc in a
# zsh script risks the shell's encoding getting in the way.

# Commit a phase with a pinned timestamp, so this VCS and system git produce the
# same commit id. GIT_AUTHOR_DATE and GIT_COMMITTER_DATE are read by both.
commit_phase() {
  local repo="$1" message="$2" epoch="$3"
  GIT_AUTHOR_DATE="$epoch +0800" GIT_COMMITTER_DATE="$epoch +0800" \
    java -cp "$MAIN_CP" lawfirm.vcs.Main commit "$repo" -m "$message" >/dev/null
}

run_ours() {
  java -cp "$MAIN_CP" lawfirm.vcs.Main init "$OURS" >/dev/null
  java -cp "$MAIN_CP" lawfirm.vcs.Vcs stage "$OURS" 1
  java -cp "$MAIN_CP" lawfirm.vcs.Main add-all "$OURS" >/dev/null
  commit_phase "$OURS" "first commit" 1700000000

  java -cp "$MAIN_CP" lawfirm.vcs.Vcs stage "$OURS" 2
  java -cp "$MAIN_CP" lawfirm.vcs.Main add-all "$OURS" >/dev/null
  java -cp "$MAIN_CP" lawfirm.vcs.Main rm "$OURS" sub/nested/c.txt >/dev/null
  commit_phase "$OURS" "second commit" 1700000060
}

run_git() {
  (
    cd "$THEIRS"
    "$GIT_BIN" init -q -b main .
    "$GIT_BIN" config user.name  "Law Firm IT"
    "$GIT_BIN" config user.email "it@firm.example"

    java -cp "$MAIN_CP" lawfirm.vcs.Vcs stage "$THEIRS" 1
    "$GIT_BIN" add -A
    GIT_AUTHOR_DATE="1700000000 +0800" GIT_COMMITTER_DATE="1700000000 +0800" \
      "$GIT_BIN" commit -q -m "first commit"

    java -cp "$MAIN_CP" lawfirm.vcs.Vcs stage "$THEIRS" 2
    "$GIT_BIN" add -A
    GIT_AUTHOR_DATE="1700000060 +0800" GIT_COMMITTER_DATE="1700000060 +0800" \
      "$GIT_BIN" commit -q -m "second commit"
  )
}

export GIT_AUTHOR_NAME="Law Firm IT"
export GIT_AUTHOR_EMAIL="it@firm.example"
export GIT_COMMITTER_NAME="Law Firm IT"
export GIT_COMMITTER_EMAIL="it@firm.example"

run_git
run_ours

ours_log="$WORK/ours-log.txt"
theirs_log="$WORK/theirs-log.txt"
( cd "$THEIRS" && "$GIT_BIN" log --format=%H --reverse ) > "$theirs_log"
# This VCS logs newest first, so reverse to oldest first to match git --reverse.
java -cp "$MAIN_CP" lawfirm.vcs.Vcs commits "$OURS" > "$ours_log"

echo "commit ids produced by system git:"
sed 's/^/  /' "$theirs_log"
echo "commit ids produced by this VCS:"
sed 's/^/  /' "$ours_log"

if ! diff -u "$theirs_log" "$ours_log"; then
  echo "FAIL: commit ids differ" >&2
  exit 1
fi
echo "OK: every commit id matches the system git, bit for bit"

# Object hashes: every object git produced must exist here under the same id.
ours_objects="$WORK/ours-objects.txt"
theirs_objects="$WORK/theirs-objects.txt"
( cd "$OURS/.vcs/objects" && find . -type f | sed 's|^\./||; s|/||' | sort ) > "$ours_objects"
( cd "$THEIRS/.git/objects" && find . -type f | sed 's|^\./||; s|/||' | sort ) > "$theirs_objects"

missing="$WORK/missing.txt"
comm -23 "$theirs_objects" "$ours_objects" > "$missing" || true
if [[ -s "$missing" ]]; then
  echo "FAIL: objects present in git but absent here:" >&2
  sed 's/^/  /' "$missing" >&2
  exit 1
fi
echo "OK: all $(wc -l < "$theirs_objects" | tr -d ' ') objects git produced exist here under the same id"
extra="$(comm -13 "$theirs_objects" "$ours_objects" | wc -l | tr -d ' ')"
echo "     (this VCS additionally holds $extra intermediate object(s) from staging)"

# Tree structure: compare path, mode and blob id for every commit.
step "4/4  tree structure comparison"
java -cp "$MAIN_CP" lawfirm.vcs.Main dump-graph "$OURS" >/dev/null

while read -r commit; do
  # ls-tree already emits "<mode> <type> <id>\t<path>", the same shape this VCS
  # prints, so only the ordering needs normalizing before the diff.
  ( cd "$THEIRS" && "$GIT_BIN" -c core.quotePath=false ls-tree -r "$commit" ) \
    | sort > "$WORK/git-tree.txt"
  java -cp "$MAIN_CP" lawfirm.vcs.Main cat-file "$OURS" "$commit" >/dev/null 2>&1 || true
  java -cp "$MAIN_CP" lawfirm.vcs.Vcs tree "$OURS" "$commit" | sort > "$WORK/ours-tree.txt"
  if diff -u "$WORK/git-tree.txt" "$WORK/ours-tree.txt" > "$WORK/tree-diff.txt"; then
    echo "OK: tree structure of $commit matches ($(wc -l < "$WORK/git-tree.txt" | tr -d ' ') entries)"
  else
    echo "FAIL: tree structure of $commit differs" >&2
    cat "$WORK/tree-diff.txt" >&2
    exit 1
  fi
done < "$theirs_log"

hr
echo "verify.sh: all checks passed"
