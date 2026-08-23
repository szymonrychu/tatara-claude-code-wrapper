"""refresh-claude-code.yml: a pushed bump must never be stranded behind no PR.

#184. The hand-rolled workflow failed on all 27 runs from 2026-07-07 to
2026-08-23 and left 27 orphan `deps/claude-code-*` branches on origin with zero
PRs between them. Two defects, and the log for run 32624497855 shows both on
one path:

  - `gh` is not on the tatara-claude-code-wrapper ARC runner image, so
    `gh pr create` exited 127. Every day.
  - The dedup guard ran `gh pr list ... | grep -q .` as an `if` CONDITION,
    where bash suspends errexit. That 127 was indistinguishable from "no PR is
    open", so the run pushed a branch and then died. The guard could never
    have matched anyway: the branch name embedded the version, so every day
    asked about a name that had never existed.

So the contract pinned here is:

  - the forge helpers and the `gh` binary both come from cd-release, which is
    where they are already tested and already installed for release.yml;
  - the branch is ONE stable name, so a failed run is retried on the same
    branch tomorrow rather than abandoned;
  - whether to push is decided on the TREE, so an unchanged bump does not
    restart the self-hosted ci matrix on the open PR every morning;
  - a forge read that FAILED is never treated as "the thing is not there";
  - the PR is never merged without the semver label ci cuts the release tag
    from.

Part A is structural, over the workflow files. Part B scans the step body out
of the YAML and runs it for REAL bash against a stubbed `gh`, a stubbed `curl`,
a real git repo with a real origin, and a test double for gh-lib.sh.
"""

import os
import re
import shutil
import subprocess
import textwrap
from pathlib import Path

import pytest

HERE = Path(__file__).parent
WORKFLOWS = HERE.parent / "workflows"
ROOT = HERE.parents[1]

REFRESH = WORKFLOWS / "refresh-claude-code.yml"
CURRENCY = WORKFLOWS / "claude-code-currency.yml"
CI = WORKFLOWS / "ci.yml"

BUMP_STEP = "open or update the claude-code bump PR"
BRANCH = "deps/claude-code"
PR_URL = "https://github.com/szymonrychu/tatara-claude-code-wrapper/pull/191"
REPO = "szymonrychu/tatara-claude-code-wrapper"


def code_lines(path):
    """Non-comment lines. The guards below are about what RUNS: the header
    comments quote the old broken forms verbatim, on purpose.
    """
    return [
        ln for ln in path.read_text(encoding="utf-8").splitlines()
        if not ln.strip().startswith("#")
    ]


def step_body(path, name):
    """The `run: |` block of one named step, dedented, ready to run in bash."""
    lines = path.read_text(encoding="utf-8").splitlines()
    starts = [i for i, ln in enumerate(lines) if ln.strip() == f"- name: {name}"]
    assert len(starts) == 1, f"expected one '{name}' step, found {len(starts)}"
    runs = [i for i in range(starts[0], len(lines)) if lines[i].strip() == "run: |"]
    assert runs, f"'{name}' has no `run: |` block"
    i = runs[0]
    indent = len(lines[i]) - len(lines[i].lstrip())
    body = []
    for ln in lines[i + 1 :]:
        if ln.strip() and (len(ln) - len(ln.lstrip())) <= indent:
            break
        body.append(ln)
    return textwrap.dedent("\n".join(body))


# --- Part A: structural ------------------------------------------------------


def test_no_workflow_url_carries_a_credential():
    """`git push -f "https://x-access-token:${TOKEN}@github.com/..."` is gone.

    gh-lib.sh:26-36 documents why GitGuardian blocked that form twice: a
    credential in a remote URL is persisted into .git/config, echoed by
    `git remote -v`, copied into reflogs, printed verbatim in git's transport
    errors, and carried in argv. `authenticate_origin` hands it to a
    credential helper instead.
    """
    paths = sorted(WORKFLOWS.glob("*.yml")) + sorted(WORKFLOWS.glob("*.yaml"))
    assert paths
    for path in paths:
        text = path.read_text(encoding="utf-8")
        assert "x-access-token" not in text, path.name
        assert not re.search(r"https://[^\s\"']*\$\{?[A-Za-z_]*TOKEN", text), path.name


def test_every_step_that_sets_errexit_also_inherits_it_into_substitutions():
    """`set -e` alone does not survive `x="$(f)"`. That is the whole defect."""
    for path in (REFRESH, CURRENCY):
        lines = path.read_text(encoding="utf-8").splitlines()
        setters = [i for i, ln in enumerate(lines) if ln.strip() == "set -euo pipefail"]
        assert setters, f"{path.name} sets errexit nowhere"
        for i in setters:
            assert lines[i + 1].strip() == "shopt -s inherit_errexit", (
                f"{path.name}:{i + 1} sets errexit without inheriting it"
            )


def test_the_bump_branch_is_one_stable_name():
    """A versioned branch name is a fresh name every day: the dedup lookup can
    never match, a failed run leaves a branch nothing later reuses, and the
    residue is one orphan per day forever. 27 of them, measured.
    """
    code = "\n".join(code_lines(REFRESH))
    assert f'branch="{BRANCH}"' in code
    assert "deps/claude-code-" not in code


def test_gh_and_the_forge_helpers_both_come_from_cd_release():
    """The missing `gh` and the missing hardening live in the same action."""
    text = REFRESH.read_text(encoding="utf-8")
    assert "szymonrychu/tatara-helmfile/.github/actions/cd-release@main" in text
    assert "mode: setup" in text
    assert 'source "$CD_RELEASE_LIB"' in text


def test_the_workflow_defines_no_forge_helpers_of_its_own():
    """One implementation, in the lib whose own repo tests it. No second copy."""
    text = REFRESH.read_text(encoding="utf-8")
    for helper in ("open_or_reuse_pr()", "arm_pr()", "retry()", "authenticate_origin()"):
        assert helper not in text, f"{helper} must stay in gh-lib.sh"


def test_no_forge_read_is_swallowed():
    """`|| true` on a read is how a 127 became "no PR is open"."""
    swallows = ("|| true", "|| :", "|| echo", "||true")
    for path in (REFRESH, CURRENCY):
        for line in path.read_text(encoding="utf-8").splitlines():
            code = line.strip()
            if code.startswith("#"):
                continue
            reads = any(
                tok in code for tok in ("gh ", "git fetch", "git ls-remote", "curl ")
            )
            if reads and any(s in code for s in swallows):
                raise AssertionError(f"{path.name}: forge read swallowed: {code}")


def test_there_is_no_pr_list_pre_guard():
    """With ONE stable branch, "some PR is open" stops being the right question:
    an open PR for 2.1.241 would make tomorrow's run for 2.1.242 exit early and
    leave the pin behind for as long as that PR sits. The only skip condition is
    main's pin already equalling npm's latest, which no failed read can imitate.
    """
    assert "gh pr list" not in "\n".join(code_lines(REFRESH))


def test_the_currency_check_runs_after_the_bump_cron():
    """A currency check scheduled BEFORE the bump reds on the lag the bump is
    about to clear, i.e. it is red during normal operation, which is how a check
    gets weakened to a warning and then deleted.
    """
    def cron_hour(path):
        m = re.search(r"cron: ['\"](\d+) (\d+) ", path.read_text(encoding="utf-8"))
        assert m, f"{path.name} has no parseable cron"
        return int(m.group(2)) * 60 + int(m.group(1))

    assert cron_hour(CURRENCY) > cron_hour(REFRESH)


def test_the_currency_workflow_opens_and_closes_its_own_issue():
    text = CURRENCY.read_text(encoding="utf-8")
    assert "check_claude_code_currency.py" in text
    assert "gh issue create" in text
    assert "gh issue close" in text
    # No `tatara` trigger label: an agent Task minted off this issue would
    # close it by hand-bumping the Dockerfile ARG, which is the exact
    # hand-management the bump cron exists to remove.
    assert "tatara" not in text.split("gh issue create")[1].split("\n")[0]
    assert "issues: write" in text


def test_ci_runs_this_suite():
    """These tests and check_claude_code_currency.py are wired into ci, on a
    hosted runner: they need python and the checkout, nothing else.
    """
    lines = CI.read_text(encoding="utf-8").splitlines()
    starts = [i for i, ln in enumerate(lines) if ln.strip() == "workflow-machinery:"]
    assert len(starts) == 1
    indent = len(lines[starts[0]]) - len(lines[starts[0]].lstrip())
    end = next(
        (j for j in range(starts[0] + 1, len(lines))
         if lines[j].strip() and not lines[j].strip().startswith("#")
         and (len(lines[j]) - len(lines[j].lstrip())) <= indent),
        len(lines),
    )
    job = "\n".join(lines[starts[0]:end])
    # Tied to the job that actually runs pytest: asserting `ubuntu-latest`
    # appears anywhere in ci.yml would stay green if this job moved onto the
    # ARC runner, which is the change the assertion exists to catch.
    assert "pytest .github" in job
    assert "runs-on: ubuntu-latest" in job


# --- Part B: run the step body for real --------------------------------------

LIB_LOG = "lib.log"

# Test double for tatara-helmfile's .github/actions/cd-release/gh-lib.sh. The
# real lib is tested in the repo that owns it; this one exists so this
# workflow's control flow can be run for real here, where that repo is not
# checked out. `retry` is a passthrough on purpose - the stubbed gh below is
# what the assertions are about.
FAKE_LIB = r"""
retry() { local label="$1"; shift; "$@"; }

authenticate_origin() { printf 'authenticate_origin %s\n' "$1" >> "$LIB_LOG"; }

pr_number() {
  local url="${1-}"
  [[ "$url" =~ ^https://[^[:space:]]+/pull/([0-9]+)$ ]] || return 1
  printf '%s\n' "${BASH_REMATCH[1]}"
}

open_or_reuse_pr() {
  printf 'open_or_reuse_pr %s %s\n' "$1" "$2" >> "$LIB_LOG"
  [ "${FAKE_OPEN_RC:-0}" = 0 ] || return 1
  printf '%s\n' "$FAKE_PR_URL"
}

# $FAKE_PR_STATE is a SEQUENCE, whitespace separated, last entry repeating, and
# NONE means the empty answer (open, not armed). The disarm and the post-merge
# re-read both read the state twice and the two answers differ, so one fixed
# value cannot express either path.
_pr_state() {
  printf '_pr_state\n' >> "$LIB_LOG"
  [ "${FAKE_STATE_RC:-0}" = 0 ] || return 1
  local n states s
  n=$(( $(cat "$STUB_DIR/state.n" 2>/dev/null || echo 0) + 1 ))
  printf '%s' "$n" > "$STUB_DIR/state.n"
  read -r -a states <<< "${FAKE_PR_STATE-}"
  [ "${#states[@]}" -eq 0 ] && { printf '\n'; return 0; }
  [ "$n" -gt "${#states[@]}" ] && n="${#states[@]}"
  s="${states[$((n - 1))]}"
  [ "$s" = NONE ] && s=""
  printf '%s\n' "$s"
}

arm_pr() {
  printf 'arm_pr %s %s %s\n' "$1" "$2" "$3" >> "$LIB_LOG"
  return "${FAKE_ARM_RC:-0}"
}

# The PR the branch already has open, as seen BEFORE this run pushes anything.
# Empty by default: most fixtures start with no PR at all.
_list_open_pr() {
  printf '_list_open_pr %s %s\n' "$1" "$2" >> "$LIB_LOG"
  [ "${FAKE_LIST_RC:-0}" = 0 ] || return 1
  printf '%s\n' "${FAKE_OPEN_PR_URL-}"
}
"""

FAKE_LIB_FUNCTIONS = set(re.findall(r"^(\S+)\(\)", FAKE_LIB, re.M))

# Scripted `gh`, keyed on the four calls this workflow makes. Each key reads
# "$STUB_DIR/<key>.plan": one line per invocation, "<exit> [stdout]"; the last
# line repeats once the plan is exhausted, and a missing plan is exit 0.
STUB_GH = r"""#!/usr/bin/env bash
if [ "$1" = api ] && printf '%s ' "$@" | grep -q ' PATCH '; then
  key=api-patch
elif [ "$1" = api ] && printf '%s ' "$@" | grep -q ' POST '; then
  key=api-label-post
elif [ "$1" = api ] && printf '%s ' "$@" | grep -q '/pulls?'; then
  key=api-pr-history
elif [ "$1" = api ]; then
  key=api-label-read
else
  key="$1-$2"
fi
printf '%s %s\n' "$key" "$*" >> "$STUB_DIR/calls.log"
plan="$STUB_DIR/$key.plan"
n_file="$STUB_DIR/$key.n"
n=$(( $(cat "$n_file" 2>/dev/null || echo 0) + 1 ))
printf '%s' "$n" > "$n_file"
[ -f "$plan" ] || exit 0
line="$(sed -n "${n}p" "$plan")"
[ -n "$line" ] || line="$(tail -n 1 "$plan")"
code="${line%% *}"
out="${line#* }"
[ "$out" = "$line" ] && out=""
[ -n "$out" ] && printf '%s\n' "$out"
exit "$code"
"""

# Scripted `curl`: prints $CURL_BODY and exits $CURL_RC.
STUB_CURL = r"""#!/usr/bin/env bash
printf 'curl %s\n' "$*" >> "$STUB_DIR/calls.log"
[ "${CURL_RC:-0}" = 0 ] || exit "${CURL_RC}"
printf '%s\n' "${CURL_BODY-}"
"""

PINNED = "2.1.201"
# One normal day's bump: inside the workflow's max_jump, so it arms.
BUMPED = "2.1.205"

# The identity the workflow commits under, and therefore the only identity whose
# work on the stable branch it may force-push over.
BOT_NAME = "szymonrychu-bot"
BOT_EMAIL = "szymonrychu-bot@users.noreply.github.com"


def npm_latest(version):
    return '{"name":"@anthropic-ai/claude-code","version":"%s","bin":{}}' % version


def dockerfile(version):
    return (
        "FROM node:22-bookworm-slim AS base\n"
        f"ARG CLAUDE_CODE_VERSION={version}\n"
        "FROM base AS runtime\n"
        "ARG CLAUDE_CODE_VERSION\n"
        "RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}\n"
    )


class Harness:
    """A bare origin, a shallow clone of it, and stubs for gh and curl."""

    def __init__(self, tmp_path):
        self.tmp = tmp_path
        self.bin = tmp_path / "bin"
        self.bin.mkdir()
        for name, text in (("gh", STUB_GH), ("curl", STUB_CURL)):
            p = self.bin / name
            p.write_text(text, encoding="utf-8")
            p.chmod(0o755)
        (tmp_path / "gh-lib.sh").write_text(FAKE_LIB, encoding="utf-8")

        self.origin = tmp_path / "origin.git"
        self._git(tmp_path, "init", "--bare", "-b", "main", str(self.origin))
        seed = tmp_path / "seed"
        seed.mkdir()
        self._git(seed, "init", "-b", "main")
        self._commit(seed, dockerfile(PINNED), "seed")
        self._git(seed, "remote", "add", "origin", self.origin_url)
        self._git(seed, "push", "origin", "main")

        self.work = tmp_path / "work"
        self._git(tmp_path, "clone", "--depth=1", self.origin_url, str(self.work))

    @property
    def origin_url(self):
        return f"file://{self.origin}"

    def _git(self, cwd, *args, name=BOT_NAME, email=BOT_EMAIL):
        return subprocess.run(
            ["git", *args],
            cwd=str(cwd),
            capture_output=True,
            text=True,
            check=True,
            env={
                **os.environ,
                "GIT_AUTHOR_NAME": name,
                "GIT_AUTHOR_EMAIL": email,
                "GIT_COMMITTER_NAME": name,
                "GIT_COMMITTER_EMAIL": email,
            },
        )

    def _commit(self, cwd, content, message, **who):
        (cwd / "Dockerfile").write_text(content, encoding="utf-8")
        self._git(cwd, "add", "Dockerfile")
        self._git(cwd, "commit", "-m", message, **who)

    def push_branch(self, version, base="main", extra=None, **who):
        """Put an existing bump branch on origin, as a prior run would have.

        `who` overrides the committing identity and `extra` adds a second file
        to the commit: between them they model the two ways the branch tip can
        stop being "main plus the bump" that this workflow is allowed to
        rebuild.
        """
        side = self.tmp / f"side-{version}"
        self._git(self.tmp, "clone", self.origin_url, str(side))
        self._git(side, "checkout", "-B", BRANCH, f"origin/{base}")
        if extra:
            (side / extra).write_text("fixup\n", encoding="utf-8")
            self._git(side, "add", extra)
        self._commit(
            side, dockerfile(version), f"chore(deps): bump claude-code to {version}", **who
        )
        self._git(side, "push", "-f", "origin", f"HEAD:{BRANCH}")
        return self.remote_sha()

    def advance_main(self, name):
        """A commit to main that has nothing to do with claude-code.

        This repo's main takes CD pin bumps regularly, so "main moved between
        two runs" is the normal case, not an exotic one. The runner checks main
        out fresh every morning, so the work clone is rebuilt too.
        """
        side = self.tmp / f"main-{name}"
        self._git(self.tmp, "clone", self.origin_url, str(side))
        (side / name).write_text("unrelated\n", encoding="utf-8")
        self._git(side, "add", name)
        self._git(side, "commit", "-m", f"chore: {name}")
        self._git(side, "push", "origin", "main")
        shutil.rmtree(self.work)
        self._git(self.tmp, "clone", "--depth=1", self.origin_url, str(self.work))

    def click_update_branch(self):
        """The "Update branch" button: a merge of main into the bump branch.

        `git diff-tree --no-commit-id --name-only -r` prints NOTHING and exits 0
        on a merge commit, so this tip passes a Dockerfile-only test by
        reporting no files at all.
        """
        side = self.tmp / "update-branch"
        self._git(self.tmp, "clone", self.origin_url, str(side))
        self._git(side, "checkout", "-B", BRANCH, f"origin/{BRANCH}")
        self._git(side, "merge", "--no-ff", "origin/main", "-m", "Merge branch 'main'")
        self._git(side, "push", "origin", f"HEAD:{BRANCH}")
        return self.remote_sha()

    def remote_sha(self):
        out = self._git(self.tmp, "ls-remote", "--heads", self.origin_url, BRANCH)
        return out.stdout.split("\t")[0] if out.stdout.strip() else ""

    def remote_dockerfile(self):
        out = self._git(self.tmp, "archive", "--remote", str(self.origin), BRANCH, "Dockerfile")
        return out.stdout

    def plan(self, key, lines):
        (self.tmp / f"{key}.plan").write_text("\n".join(lines) + "\n", encoding="utf-8")

    def calls(self, key=None):
        log = self.tmp / "calls.log"
        if not log.exists():
            return []
        got = [ln for ln in log.read_text(encoding="utf-8").splitlines() if ln]
        return got if key is None else [ln for ln in got if ln.startswith(key + " ")]

    def lib_calls(self):
        log = self.tmp / LIB_LOG
        return log.read_text(encoding="utf-8").splitlines() if log.exists() else []

    def run(self, latest=BUMPED, curl_rc=0, **env):
        script = (
            "set -euo pipefail\n"
            "shopt -s inherit_errexit\n"
            + step_body(REFRESH, BUMP_STEP)
        )
        return subprocess.run(
            ["bash", "-c", script],
            cwd=str(self.work),
            capture_output=True,
            text=True,
            check=False,
            env={
                **os.environ,
                "PATH": f"{self.bin}:{os.environ['PATH']}",
                "STUB_DIR": str(self.tmp),
                "LIB_LOG": str(self.tmp / LIB_LOG),
                "CD_RELEASE_LIB": str(self.tmp / "gh-lib.sh"),
                "CURL_BODY": npm_latest(latest),
                "CURL_RC": str(curl_rc),
                "REPO": REPO,
                "GITHUB_REF_NAME": "main",
                "GH_TOKEN": "not-a-real-token",
                "FAKE_PR_URL": PR_URL,
                **{k: str(v) for k, v in env.items()},
            },
        )


@pytest.fixture
def h(tmp_path):
    return Harness(tmp_path)


def test_the_double_models_exactly_the_helpers_the_workflow_calls():
    """A double that carries a helper the workflow no longer calls, or misses
    one it does, makes every Part B result meaningless.
    """
    # Comments strip out first: `arm_pr` and `_pr_state` are both named in
    # comments, so matching the raw body would keep this green after the
    # workflow stopped calling them.
    body = "\n".join(
        ln for ln in step_body(REFRESH, BUMP_STEP).splitlines()
        if not ln.strip().startswith("#")
    )
    called = {f for f in FAKE_LIB_FUNCTIONS if re.search(rf"(?<![\w-]){f}\b", body)}
    assert called == FAKE_LIB_FUNCTIONS


@pytest.mark.skipif(
    not (ROOT.parent / "tatara-helmfile" / ".github/actions/cd-release/gh-lib.sh").exists(),
    reason="tatara-helmfile is not checked out beside this repo",
)
def test_every_helper_the_double_models_exists_in_the_real_lib():
    """The other direction of the same drift: the workflow may only call names
    gh-lib.sh actually defines. Runs wherever both repos are on disk.
    """
    real = (ROOT.parent / "tatara-helmfile" / ".github/actions/cd-release/gh-lib.sh").read_text(
        encoding="utf-8"
    )
    defined = set(re.findall(r"^(\S+)\(\)", real, re.M))
    assert FAKE_LIB_FUNCTIONS <= defined, FAKE_LIB_FUNCTIONS - defined


def test_a_pin_already_at_latest_touches_nothing(h):
    r = h.run(latest=PINNED)
    assert r.returncode == 0, r.stderr
    assert h.remote_sha() == ""
    assert h.lib_calls() == []


def test_an_unreadable_registry_is_not_a_current_pin(h):
    """Fail-closed: the one thing a failed read must never mean is "up to date"."""
    r = h.run(curl_rc=22)
    assert r.returncode != 0
    assert h.remote_sha() == ""


def test_an_unparseable_registry_answer_fails_loudly(h):
    r = h.run()
    assert r.returncode == 0
    r = h.run(CURL_BODY="<html>502 Bad Gateway</html>")
    assert r.returncode != 0
    assert "could not" in (r.stdout + r.stderr)


def test_a_first_run_pushes_the_branch_and_arms_the_pr(h):
    r = h.run()
    assert r.returncode == 0, r.stdout + r.stderr
    assert h.remote_sha() != ""
    assert f"ARG CLAUDE_CODE_VERSION={BUMPED}" in h.remote_dockerfile()
    assert "ARG CLAUDE_CODE_VERSION\n" in h.remote_dockerfile(), (
        "the bare re-declaration in the claude layer must survive the rewrite"
    )
    log = h.lib_calls()
    assert f"authenticate_origin {REPO}" in log
    assert any(ln.startswith(f"open_or_reuse_pr {REPO} {BRANCH}") for ln in log)
    assert any(ln.startswith("arm_pr") for ln in log)


def test_an_identical_tree_is_not_re_pushed(h):
    """`git commit` mints a new sha for an identical tree. Force-pushing that
    every morning restarts the whole self-hosted ci matrix on the open PR, so
    the push is decided on the tree, not on the commit.
    """
    before = h.push_branch(BUMPED)
    r = h.run()
    assert r.returncode == 0, r.stdout + r.stderr
    assert h.remote_sha() == before
    assert any(ln.startswith("open_or_reuse_pr") for ln in h.lib_calls()), (
        "an unchanged tree still has to reach the PR: the previous run may have "
        "died before opening or arming it"
    )


def test_a_stale_branch_is_rebuilt(h):
    before = h.push_branch("2.1.198")
    r = h.run()
    assert r.returncode == 0, r.stdout + r.stderr
    assert h.remote_sha() != before
    assert f"ARG CLAUDE_CODE_VERSION={BUMPED}" in h.remote_dockerfile()


def test_a_failed_branch_read_is_not_treated_as_an_absent_branch(h):
    """The read-side rule, one level down from the guard that caused #184: an
    ls-remote that FAILED must not license a force-push over a branch this run
    could not see.
    """
    h.push_branch(BUMPED)
    before = h.remote_sha()
    # Blocks the remote transport only; the local checkout/commit still work,
    # so the run reaches ls-remote and dies THERE. Asserting on that specific
    # diagnosis is what separates "the guard fired" from "something else broke
    # before the push happened to be reached".
    r = h.run(GIT_ALLOW_PROTOCOL="none")
    assert r.returncode != 0
    assert h.remote_sha() == before
    assert "refusing to force-push over a branch this run never saw" in (r.stdout + r.stderr)


def test_a_pr_that_cannot_be_opened_fails_the_run(h):
    r = h.run(FAKE_OPEN_RC=1)
    assert r.returncode != 0
    assert h.remote_sha() != "", "the push already happened; that is the point"


def test_a_reused_pr_gets_its_title_and_body_rewritten(h):
    """The title becomes the squash commit message, and a reused PR keeps
    yesterday's version in it.
    """
    r = h.run()
    assert r.returncode == 0, r.stdout + r.stderr
    patch = h.calls("api-patch")
    assert len(patch) == 1, h.calls()
    assert "repos/szymonrychu/tatara-claude-code-wrapper/pulls/191" in patch[0]
    assert BUMPED in patch[0]


def test_a_failed_title_rewrite_fails_the_run(h):
    h.plan("api-patch", ["1"])
    r = h.run()
    assert r.returncode != 0
    assert "could not" in (r.stdout + r.stderr)


def recovered(h):
    """The crash-recovery state: a prior run pushed the branch and died before
    arming, so today's run rebuilds an IDENTICAL tree and does not push.
    """
    h.push_branch(BUMPED)


def test_an_already_armed_pr_is_not_re_armed(h):
    recovered(h)
    r = h.run(FAKE_PR_STATE="ARMED")
    assert r.returncode == 0, r.stdout + r.stderr
    assert not any(ln.startswith("arm_pr") for ln in h.lib_calls())


def test_an_armed_read_is_not_trusted_after_this_run_moved_the_head(h):
    """GitHub disables auto-merge when the head branch is force-pushed, and the
    field is not synchronously consistent with a ref update seconds old. A stale
    ARMED believed here leaves a green PR that never merges, i.e. #184 again.
    """
    h.push_branch("2.1.198")
    r = h.run(FAKE_PR_STATE="ARMED")
    assert r.returncode == 0, r.stdout + r.stderr
    assert any(ln.startswith("arm_pr") for ln in h.lib_calls()), (
        "a force-pushed head must be re-armed, not trusted"
    )


def test_an_already_merged_pr_is_a_success(h):
    r = h.run(FAKE_PR_STATE="MERGED")
    assert r.returncode == 0, r.stdout + r.stderr
    assert not any(ln.startswith("arm_pr") for ln in h.lib_calls())


def test_an_unreadable_pr_state_fails_the_run(h):
    r = h.run(FAKE_STATE_RC=1)
    assert r.returncode != 0
    assert not any(ln.startswith("arm_pr") for ln in h.lib_calls())


def test_a_clean_labelled_pr_that_auto_merge_refuses_is_merged_outright(h):
    """enablePullRequestAutoMerge refuses a PR with nothing left pending, which
    is exactly the state a run that died between the push and the arm leaves
    behind: open, green, unarmed, and never merging on its own. That is the
    47-day shape this redesign exists to end, so it cannot be a daily red.
    """
    recovered(h)
    h.plan("pr-view", ["0 CLEAN"])
    h.plan("api-label-read", ["0 area/cd,semver:patch"])
    r = h.run(FAKE_ARM_RC=1)
    assert r.returncode == 0, r.stdout + r.stderr
    assert h.calls("pr-merge"), "a clean PR auto-merge refused must be merged directly"


def test_a_pr_this_run_just_pushed_is_never_merged_outright(h):
    """The fallback is for a PR an EARLIER run left behind, which by definition
    has an unchanged tree. On a head this run just moved, the checks are pending
    and `--auto` is the right mechanism; merging outright would merge something
    no run has ever seen go green. CLEAN alone does not distinguish the two.
    """
    h.plan("pr-view", ["0 CLEAN"])
    h.plan("api-label-read", ["0 semver:patch"])
    r = h.run(FAKE_ARM_RC=1)
    assert r.returncode != 0
    assert not h.calls("pr-merge")


def test_a_pr_that_is_not_mergeable_is_not_merged_outright(h):
    recovered(h)
    h.plan("pr-view", ["0 BLOCKED"])
    r = h.run(FAKE_ARM_RC=1)
    assert r.returncode != 0
    assert not h.calls("pr-merge")


def test_an_unlabelled_pr_is_never_merged_outright(h):
    """ci cuts the release tag from the semver label, so a merge that lands
    without it publishes an image nothing ever tags. arm_pr returns the same
    status for a failed label POST as for a failed arm, so the label has to be
    re-read here rather than assumed.
    """
    recovered(h)
    h.plan("pr-view", ["0 CLEAN"])
    h.plan("api-label-read", ["0 area/cd"])
    r = h.run(FAKE_ARM_RC=1)
    assert r.returncode != 0
    assert not h.calls("pr-merge")
    assert "semver:patch" in (r.stdout + r.stderr)


def test_an_unreadable_merge_state_is_not_treated_as_mergeable(h):
    recovered(h)
    h.plan("pr-view", ["1"])
    r = h.run(FAKE_ARM_RC=1)
    assert r.returncode != 0
    assert not h.calls("pr-merge")


# --- the catch-up guard ------------------------------------------------------


def test_a_catch_up_jump_is_labelled_but_left_unarmed(h):
    """#184 pre-mortem 1: 2.1.201 to 2.1.241 is 40 releases onto every agent pod
    in the fleet. `ci` sets enable-image: false and builds no image, so a green
    run on a Dockerfile-only PR is not evidence the new claude-code installs -
    `release` is the first thing that builds it, and it cuts the semver tag
    BEFORE the build, so a bad bump leaves a tag with no image behind it.
    """
    r = h.run(latest="2.1.241")
    assert r.returncode == 0, r.stdout + r.stderr
    assert h.remote_sha() != "", "the PR is still opened; only the arming stops"
    assert not any(ln.startswith("arm_pr") for ln in h.lib_calls())
    assert h.calls("api-label-post"), (
        "an unarmed PR still needs the semver label: a human merging it "
        "unlabelled publishes an image release refuses to tag"
    )
    assert not h.calls("pr-merge")


def test_a_minor_bump_is_left_unarmed(h):
    r = h.run(latest="2.2.0")
    assert r.returncode == 0, r.stdout + r.stderr
    assert not any(ln.startswith("arm_pr") for ln in h.lib_calls())


def test_a_bump_inside_the_jump_budget_is_armed(h):
    """The steady state has to keep working, or the guard has replaced a dead
    cron with a cron nobody ever merges.
    """
    r = h.run(latest="2.1.206")
    assert r.returncode == 0, r.stdout + r.stderr
    assert any(ln.startswith("arm_pr") for ln in h.lib_calls())


def test_a_prerelease_latest_is_refused_outright(h):
    """dist-tags.latest is a pointer and nothing guarantees it names a plain
    release. The currency check's version filter drops a prerelease too, so
    pinning one would also red that check with a diagnosis blaming npm.
    """
    r = h.run(latest="2.2.0-rc.1")
    assert r.returncode != 0
    assert h.remote_sha() == ""
    assert "not a plain X.Y.Z release" in (r.stdout + r.stderr)


def test_a_failed_label_post_stops_before_arming(h):
    h.plan("api-label-post", ["1"])
    r = h.run()
    assert r.returncode != 0
    assert not any(ln.startswith("arm_pr") for ln in h.lib_calls())


def test_a_catch_up_bump_disarms_a_pr_an_earlier_run_armed(h):
    """Declining to arm is not disarming, and the PR is not necessarily new.

    Day 1 bumps 4 releases and arms PR #N. `ci` stalls - the failure class this
    whole issue is about ran 47 days. npm keeps publishing, and the first day
    the delta crosses max_jump this run force-pushes a catch-up head onto that
    same PR. If it merely declines to arm, the arm from day 1 is still there
    and squashes the catch-up onto every agent pod the moment ci recovers.
    """
    before = h.push_branch("2.1.198")
    r = h.run(latest="2.1.241", FAKE_OPEN_PR_URL=PR_URL, FAKE_PR_STATE="NONE")
    assert r.returncode == 0, r.stdout + r.stderr
    disarm = [c for c in h.calls("pr-merge") if "--disable-auto" in c]
    assert len(disarm) == 1, h.calls()
    assert h.remote_sha() != before, "a disarmed PR may have its head moved"
    assert not any(ln.startswith("arm_pr") for ln in h.lib_calls())


def test_the_disarm_happens_before_the_head_moves(h):
    """`arm` is fully decided before the checkout, so there is no reason to put
    the push and four retry-wrapped forge calls between a catch-up head and the
    removal of an arm an earlier run put on that PR - up to ~50s of backoff each
    under a partial outage, with auto-merge live on the new head throughout.
    """
    h.push_branch("2.1.198")
    r = h.run(latest="2.1.241", FAKE_OPEN_PR_URL=PR_URL, FAKE_PR_STATE="NONE")
    assert r.returncode == 0, r.stdout + r.stderr
    order = [c.split()[0] for c in h.calls()]
    assert "pr-merge" in order and "api-patch" in order
    assert order.index("pr-merge") < order.index("api-patch"), h.calls()


def test_a_catch_up_pr_is_disarmed_even_when_the_state_reads_unarmed(h):
    """The disarm may not be gated on `_pr_state` saying ARMED.

    That is the same field the ARMED case below refuses to believe, and an empty
    answer is the same staleness in the other direction: it would skip the
    disarm AND the read-back and exit 0 on a head that is still armed. The call
    errors on a PR that was never armed, which is why ITS failure is tolerated
    and the read-back is the observable.
    """
    h.push_branch("2.1.198")
    h.plan("pr-merge", ["1"])
    r = h.run(latest="2.1.241", FAKE_OPEN_PR_URL=PR_URL, FAKE_PR_STATE="NONE")
    assert r.returncode == 0, r.stdout + r.stderr
    assert [c for c in h.calls("pr-merge") if "--disable-auto" in c]


def test_a_disarm_that_does_not_take_leaves_the_head_where_it_was(h):
    """`gh pr merge --disable-auto` exiting 0 is not evidence of anything, the
    same way `--auto` exiting 0 is not (gh-lib.sh:199-202). The observable is
    the read-back, and the head must not move onto a PR still armed.
    """
    before = h.push_branch("2.1.198")
    r = h.run(latest="2.1.241", FAKE_OPEN_PR_URL=PR_URL, FAKE_PR_STATE="ARMED")
    assert r.returncode != 0
    assert "still armed" in (r.stdout + r.stderr)
    assert h.remote_sha() == before


def test_an_unlistable_pr_is_not_an_absent_arm(h):
    """A list that FAILED must never license moving the head of a PR this run
    could not see the arming state of.
    """
    before = h.push_branch("2.1.198")
    r = h.run(latest="2.1.241", FAKE_LIST_RC=1)
    assert r.returncode != 0
    assert h.remote_sha() == before


def test_a_catch_up_that_merges_mid_disarm_does_not_claim_a_disarm(h):
    """Nothing here can undo it, but the log must not report a removal that
    never happened, and this run's view of main is now stale.
    """
    before = h.push_branch("2.1.198")
    r = h.run(latest="2.1.241", FAKE_OPEN_PR_URL=PR_URL, FAKE_PR_STATE="MERGED")
    assert r.returncode == 0, r.stdout + r.stderr
    assert "merged while this run was disarming it" in (r.stdout + r.stderr)
    assert "auto-merge is off" not in (r.stdout + r.stderr)
    assert h.remote_sha() == before


def test_a_refused_bump_that_merged_anyway_is_not_an_ordinary_success(h):
    """The MERGED short-circuit used to sit ABOVE the arm=no gate, so a bump
    this run deliberately refused to arm, which merged anyway between the disarm
    and this read, was logged as a plain green success with no mention of the
    jump - indistinguishable from a hand-merge, and the run cannot tell them
    apart.
    """
    r = h.run(latest="2.1.241", FAKE_PR_STATE="MERGED")
    assert r.returncode == 0, r.stdout + r.stderr
    out = r.stdout + r.stderr
    assert "::warning::" in out
    assert "40 patch releases at once" in out


def test_a_pr_that_reads_armed_after_the_push_is_disarmed_again(h):
    """After the push, erroring is a REPORT, not a refusal.

    The pre-push twin refuses to move the head and does. This one runs after the
    head has already moved, so exiting non-zero here would leave a catch-up head
    live under auto-merge and call that a guard - the run reds and the fleet
    still gets 40 releases the moment ci goes green. Reachable two ways: the
    pre-push read-back was stale, or something armed the PR in the window (a
    human clicking "Enable auto-merge" on the PR yesterday's run told them to
    merge by hand). Both are answered by issuing the removal again.
    """
    h.push_branch("2.1.198")
    r = h.run(
        latest="2.1.241", FAKE_OPEN_PR_URL=PR_URL, FAKE_PR_STATE="NONE ARMED NONE"
    )
    assert r.returncode == 0, r.stdout + r.stderr
    disarm = [c for c in h.calls("pr-merge") if "--disable-auto" in c]
    assert len(disarm) == 2, h.calls()


def test_a_pr_still_armed_after_two_removals_fails_the_run(h):
    h.push_branch("2.1.198")
    r = h.run(latest="2.1.241", FAKE_OPEN_PR_URL=PR_URL, FAKE_PR_STATE="NONE ARMED")
    assert r.returncode != 0
    assert "still armed" in (r.stdout + r.stderr)


def test_only_the_escalated_disarm_is_retried():
    """The pre-push removal is issued unconditionally, so it is EXPECTED to fail
    on every steady catch-up or veto day - the PR is unarmed and stays unarmed.
    Under gh-lib's `retry` that is 5+10+15+20 = 50s of backoff in front of the
    force-push, every morning, for a call whose failure is already declared
    meaningless - the exact cost this block was moved above the push to avoid.
    The read-back is the observable. The post-push removal IS retried: it is
    issued only when a read already said ARMED, so a failure there is real.

    Structural because the Part B double's `retry` is a passthrough, so no run
    of this suite can tell a retried call from a bare one.
    """
    # Comments strip out first: both blocks discuss `--disable-auto` in prose,
    # and so does each block's own ::warning::.
    lines = [
        ln for ln in step_body(REFRESH, BUMP_STEP).splitlines()
        if not ln.strip().startswith("#")
    ]
    at = [
        i for i, ln in enumerate(lines)
        if "--disable-auto" in ln and re.match(r"(if ! )?gh pr merge\b", ln.lstrip())
    ]
    assert len(at) == 2, at
    pre, post = at
    # The previous line too: `retry` wraps across a line continuation.
    assert 'retry "disarm' not in lines[pre - 1] + lines[pre]
    assert 'retry "disarm' in lines[post - 1] + lines[post]


def test_an_unarmed_catch_up_pr_with_no_open_pr_is_not_disarmed(h):
    """Nothing to disarm when the branch has no PR at all: the one this run is
    about to open cannot be armed, because this run is what would arm it.
    """
    r = h.run(latest="2.1.241")
    assert r.returncode == 0, r.stdout + r.stderr
    assert not h.calls("pr-merge")


def test_a_downgrade_is_never_armed(h):
    """The jump budget was a SIGNED comparison: `$(( 150 - 201 ))` is -51, which
    is not `-gt 5`, so npm re-pointing dist-tags.latest back after a bad line
    would have shipped 51 releases of rollback to the fleet unreviewed. The
    stated intent is "a bump the size of a normal day's", and -51 is not that.
    """
    r = h.run(latest="2.1.150")
    assert r.returncode == 0, r.stdout + r.stderr
    assert not any(ln.startswith("arm_pr") for ln in h.lib_calls())
    assert h.calls("api-label-post"), "the PR is still opened and labelled"


# --- whose branch is it --------------------------------------------------------


def test_a_human_commit_on_the_branch_is_not_force_pushed_over(h):
    """The arm=no path tells a human to "review and merge it by hand" on a
    branch with a fixed, well-known name. One fixup commit there (a compat tweak
    the new claude-code needs) would be discarded by the next `checkout -B` off
    main and overwritten by the force-push, silently turning the PR back into a
    bump-only diff while keeping its label and review context.
    """
    before = h.push_branch("2.1.198", name="a-human", email="human@example.com")
    r = h.run()
    assert r.returncode != 0
    assert h.remote_sha() == before
    assert "human@example.com" in (r.stdout + r.stderr)


def test_a_branch_tip_touching_more_than_the_dockerfile_is_not_force_pushed_over(h):
    """Bot-authored is not enough on its own: the ref this run may rebuild is
    "main plus the bump", and a second file in the tip commit means it is not.
    """
    before = h.push_branch("2.1.198", extra="compat.patch")
    r = h.run()
    assert r.returncode != 0
    assert h.remote_sha() == before
    assert "compat.patch" in (r.stdout + r.stderr)


def test_an_update_branch_merge_tip_is_named_as_a_merge(h):
    """`git diff-tree --no-commit-id --name-only -r <merge>` prints NOTHING and
    exits 0, so a merge tip passes a Dockerfile-only test by reporting no files
    at all - and clicking "Update branch" on the bump PR is the likeliest way
    this branch stops being main plus the bump. Diagnosed as "changed no files"
    it reads like an empty branch worth deleting, which is the one remedy that
    makes things worse.
    """
    before = h.push_branch("2.1.198")
    h.advance_main("unrelated.txt")
    before = h.click_update_branch()
    r = h.run()
    assert r.returncode != 0
    assert h.remote_sha() == before
    out = r.stdout + r.stderr
    assert "merge commit" in out
    assert "files ''" not in out


def test_the_ownership_refusal_names_what_deleting_the_branch_costs(h):
    """Deleting the head branch of an open PR closes that PR UNMERGED, and a
    closed-unmerged PR is this workflow's standing rejection - so the obvious
    remedy silently stops the cron arming until a human merges a bump by hand.
    Advising it without saying so is the same shape as the guard that started
    #184: a message a reader acts on that means something else.
    """
    before = h.push_branch("2.1.198", name="a-human", email="human@example.com")
    r = h.run()
    assert r.returncode != 0
    assert h.remote_sha() == before
    assert "closes the open PR unmerged" in (r.stdout + r.stderr)


def test_a_dispatch_from_a_non_main_ref_is_refused(h):
    """workflow_dispatch accepts any ref and actions/checkout takes the
    dispatched one, so without this the `checkout -B` branches off a task branch
    and the force-push replaces the canonical bump branch with it - then opens a
    labelled, armed PR whose diff is the whole feature branch. Dispatching this
    workflow from a task branch to test it is the obvious thing to do.
    """
    r = h.run(GITHUB_REF_NAME="tatara/feat-184")
    assert r.returncode != 0
    assert h.remote_sha() == ""
    assert h.lib_calls() == []


def test_an_unset_ref_name_is_refused(h):
    r = h.run(GITHUB_REF_NAME="")
    assert r.returncode != 0
    assert h.remote_sha() == ""


# --- closing the PR is a veto --------------------------------------------------


def closed(version, number=191):
    """The api-pr-history projection for one closed-unmerged bump PR.

    The version is read out of the TITLE, which this workflow rewrites on every
    run - the rejected artifact is what a close rejects.
    """
    return f"0 {number} - chore(deps): bump claude-code to {version}"


def test_a_closed_pr_is_not_reopened_on_an_identical_bump(h):
    """`_list_open_pr` filters --state open and `current` comes from main, which
    a close does not change, so nothing else in this workflow can see the
    rejection: a human who closes an armed bump because it wedged agent pods
    gets a fresh PR on the byte-identical head at 06:17, armed again.
    """
    before = h.push_branch(BUMPED)
    h.plan("api-pr-history", [closed(BUMPED)])
    r = h.run()
    assert r.returncode == 0, r.stdout + r.stderr
    assert h.remote_sha() == before
    assert not any(ln.startswith("open_or_reuse_pr") for ln in h.lib_calls())
    assert not any(ln.startswith("arm_pr") for ln in h.lib_calls())


def test_a_closed_pr_survives_an_unrelated_commit_to_main(h):
    """The veto is about the VERSION a human rejected, not about the tree.

    Gated on tree equality it only held while main was frozen: main's tip plus
    the bump changes whenever anything at all lands on main - and this repo's
    main takes CD pin bumps regularly - so the byte-identical rejected bump came
    back as a fresh PR, one per commit to main, each needing another close.
    """
    before = h.push_branch(BUMPED)
    h.advance_main("unrelated.txt")
    h.plan("api-pr-history", [closed(BUMPED)])
    r = h.run()
    assert r.returncode == 0, r.stdout + r.stderr
    assert h.remote_sha() == before
    assert not any(ln.startswith("open_or_reuse_pr") for ln in h.lib_calls())


def test_the_veto_message_does_not_advise_reopening_the_rejected_pr(h):
    """Reopening the closed PR takes it out of `select(.state == "closed")`, so
    the next run sees no veto at all and arms the byte-identical version the
    human rejected - the delta was in budget the day it was opened, which is why
    it was armed and why there was something to reject. Advising it is round 2's
    own finding 1 one branch over: a remedy whose stated effect and real effect
    differ.
    """
    before = h.push_branch(BUMPED)
    h.plan("api-pr-history", [closed(BUMPED)])
    r = h.run()
    assert r.returncode == 0, r.stdout + r.stderr
    assert h.remote_sha() == before
    out = r.stdout + r.stderr
    assert ", or reopen #" not in out, "reopening is not a remedy; it erases the veto"
    assert "would arm it" in out, "what reopening actually does has to be named"


def test_a_veto_whose_version_cannot_be_read_stops_the_cron(h):
    """An unreadable rejection scope is the WIDEST one. A hand-edited title is
    the only way here, the currency check reds within 3 days if it ever wedges
    the pin, and the alternative is guessing that a rejection a run cannot size
    does not apply to what it is about to open.
    """
    h.plan("api-pr-history", ["0 191 - please stop bumping this"])
    r = h.run()
    assert r.returncode == 0, r.stdout + r.stderr
    assert h.remote_sha() == ""
    assert not any(ln.startswith("open_or_reuse_pr") for ln in h.lib_calls())


def test_a_closed_pr_leaves_the_next_bump_unarmed(h):
    """npm moving on is a new artifact and gets a new PR, but the rejection
    still stands until a bump actually merges: re-arming the day after a human
    closed one is the same surprise one version along.
    """
    h.plan("api-pr-history", [closed("2.1.202")])
    r = h.run()
    assert r.returncode == 0, r.stdout + r.stderr
    assert h.remote_sha() != ""
    assert any(ln.startswith("open_or_reuse_pr") for ln in h.lib_calls())
    assert not any(ln.startswith("arm_pr") for ln in h.lib_calls())
    assert "191" in (r.stdout + r.stderr)


def test_a_veto_on_an_older_version_still_reconciles_the_open_pr(h):
    """The unarmed PR for the CURRENT bump is already open and neither npm nor
    main has moved. The run used to exit above the title PATCH and the label
    POST telling the reader to "reopen" a PR that was never closed - so an open
    PR's semver:patch went unreconciled for as long as that state lasted, and ci
    cuts the release tag from that label.
    """
    h.push_branch(BUMPED)
    h.plan("api-pr-history", [closed("2.1.202")])
    r = h.run(FAKE_OPEN_PR_URL=PR_URL)
    assert r.returncode == 0, r.stdout + r.stderr
    assert h.calls("api-patch"), "the reused PR's title still names the version"
    assert h.calls("api-label-post")
    assert not any(ln.startswith("arm_pr") for ln in h.lib_calls())


def test_a_catch_up_under_a_veto_reports_both_reasons(h):
    """A run that is both a 40-release catch-up and vetoed used to print only
    the veto: the human is told to merge by hand with no hint the diff spans 40
    upstream releases.
    """
    h.plan("api-pr-history", [closed("2.1.202")])
    r = h.run(latest="2.1.241")
    assert r.returncode == 0, r.stdout + r.stderr
    out = r.stdout + r.stderr
    assert "40 patch releases at once" in out
    assert "191" in out


def test_a_merged_pr_is_not_a_veto(h):
    """The steady state: the newest completed PR on the branch is the one that
    merged yesterday, and the guard has to expire on it or it has replaced a
    dead cron with a cron nobody ever merges.
    """
    h.plan(
        "api-pr-history",
        [f"0 191 2026-08-22T06:31:00Z chore(deps): bump claude-code to {BUMPED}"],
    )
    r = h.run()
    assert r.returncode == 0, r.stdout + r.stderr
    assert any(ln.startswith("arm_pr") for ln in h.lib_calls())


def test_an_unreadable_pr_history_is_not_an_absent_veto(h):
    h.plan("api-pr-history", ["1"])
    r = h.run()
    assert r.returncode != 0
    assert h.remote_sha() == ""


def test_a_squash_that_landed_on_a_timeout_is_not_a_failure(h):
    """`gh pr merge --squash` is the one non-idempotent verb under `retry`: the
    merge can land server-side while the client times out, and attempts 2-5 then
    each get "not mergeable". Reporting that as a red run means the one workflow
    whose red is supposed to mean "the pin is stranded" cries wolf. gh-lib.sh:
    171-181 already solved the same asymmetry for `gh pr create`.
    """
    recovered(h)
    h.plan("pr-view", ["0 CLEAN"])
    h.plan("api-label-read", ["0 semver:patch"])
    h.plan("pr-merge", ["1"])
    r = h.run(FAKE_ARM_RC=1, FAKE_PR_STATE="NONE MERGED")
    assert r.returncode == 0, r.stdout + r.stderr
    assert "merged" in (r.stdout + r.stderr)


def test_a_squash_that_did_not_land_still_fails_the_run(h):
    recovered(h)
    h.plan("pr-view", ["0 CLEAN"])
    h.plan("api-label-read", ["0 semver:patch"])
    h.plan("pr-merge", ["1"])
    r = h.run(FAKE_ARM_RC=1, FAKE_PR_STATE="NONE")
    assert r.returncode != 0
