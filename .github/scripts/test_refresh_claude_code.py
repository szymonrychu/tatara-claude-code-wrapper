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

_pr_state() {
  printf '_pr_state\n' >> "$LIB_LOG"
  [ "${FAKE_STATE_RC:-0}" = 0 ] || return 1
  printf '%s\n' "${FAKE_PR_STATE-}"
}

arm_pr() {
  printf 'arm_pr %s %s %s\n' "$1" "$2" "$3" >> "$LIB_LOG"
  return "${FAKE_ARM_RC:-0}"
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

    def _git(self, cwd, *args):
        return subprocess.run(
            ["git", *args],
            cwd=str(cwd),
            capture_output=True,
            text=True,
            check=True,
            env={
                **os.environ,
                "GIT_AUTHOR_NAME": "seed",
                "GIT_AUTHOR_EMAIL": "seed@example.com",
                "GIT_COMMITTER_NAME": "seed",
                "GIT_COMMITTER_EMAIL": "seed@example.com",
            },
        )

    def _commit(self, cwd, content, message):
        (cwd / "Dockerfile").write_text(content, encoding="utf-8")
        self._git(cwd, "add", "Dockerfile")
        self._git(cwd, "commit", "-m", message)

    def push_branch(self, version, base="main"):
        """Put an existing bump branch on origin, as a prior run would have."""
        side = self.tmp / f"side-{version}"
        self._git(self.tmp, "clone", self.origin_url, str(side))
        self._git(side, "checkout", "-B", BRANCH, f"origin/{base}")
        self._commit(side, dockerfile(version), f"chore(deps): bump claude-code to {version}")
        self._git(side, "push", "-f", "origin", f"HEAD:{BRANCH}")
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
