#!/usr/bin/env python3
"""Currency check for the Dockerfile's pinned claude-code version.

refresh-claude-code.yml is the daily cron that bumps
ARG CLAUDE_CODE_VERSION in the Dockerfile to the newest published
@anthropic-ai/claude-code release. It failed silently on all 27 runs since
2026-07-07 and nobody noticed for 47 days: the pin sat at 2.1.201 while npm
reached 2.1.241. Nothing checked that the cron was actually landing bumps.

This closes that blind spot by reading the npm packument directly and
redding when the pin falls more than MAX_LAG published releases behind
dist-tags.latest.

ANCHOR ON dist-tags.latest, NOT ON THE NEWEST PUBLISHED VERSION. This package
carries three dist-tags, measured 2026-08-23: {stable: 2.1.231, latest:
2.1.241, next: 2.1.241}. They are independent pointers into one publish
stream, and nothing makes `latest` the newest of them - `stable` is 10
releases behind it today, and `next` is free to move ahead of it. `latest` is
what refresh-claude-code.yml writes into the pin, so `latest` is the only
correct target for the lag computation: measuring against the newest published
version instead would count releases the pin was never supposed to track. A
pin ahead of `latest` (negative lag) is clean, not a problem.

MAX_LAG = 2, not 1 as in check_skills_currency.py's tag lag. Two separate
things had to be measured for that number to hold, because the pin this reads
is main's, so it only moves when the bump PR MERGES:

  - publish rate. claude-code publishes several times a day, clustered
    16:00-02:00Z. Over the last 60 releases the lag introduced in the
    06:17Z-09:17Z window (the bump cron to this check) was 0 on 49 of 50 days
    and 1 on the remaining one.
  - merge latency, which is the assumption the first measurement does NOT
    cover. Over the 45 auto-merged CD bump PRs in this repo: median 7.0 min,
    p90 19.9 min, max 52.9 min, none over 180. So the three-hour window
    covers the merge with a 3.4x margin on the worst observed case.

It therefore takes roughly 2-3 consecutive dead cron days to red, which is the
point: this check reds for drift, never for a bump in flight. If ARC capacity
ever pushes the merge past 09:17 the check reds on a working cron, so the
filed issue points at the open PR first.

PIN_RE anchors on the trailing `=`, not just the ARG name. Dockerfile:17 is
`ARG CLAUDE_CODE_VERSION=2.1.201`, but Dockerfile:106 re-declares the same ARG
name bare (`ARG CLAUDE_CODE_VERSION`) inside the claude layer so the build arg
crosses the FROM boundary. A name-only regex finds both and the "exactly one
match" convention mirrored from check_skills_currency.py would be permanently
red.

FAIL-CLOSED. A fetch that fails, a registry with no releases, a missing
dist-tags.latest, a latest that names no published release, an unreadable
pin, or a pin naming no published release are all reported. A currency check
that silently stops checking is worse than no currency check.

Exit 0 clean, 1 with a report.
"""

import json
import re
import sys
import urllib.request
from pathlib import Path

PACKAGE = "@anthropic-ai/claude-code"
PACKUMENT_URL = "https://registry.npmjs.org/@anthropic-ai/claude-code"

# Lag, in published npm releases, that this check tolerates. See "MAX_LAG = 2"
# above for the measurement behind this number.
MAX_LAG = 2

PIN_RE = re.compile(r"^ARG CLAUDE_CODE_VERSION=(\S+)$", re.MULTILINE)
SEMVER_RE = re.compile(r"^\d+\.\d+\.\d+$")


class FetchError(RuntimeError):
    """The packument could not be read. Never degraded into a partial dict."""


# The ABBREVIATED packument (448 KB against 1.36 MB for the full one, measured
# 2026-08-23). It carries dist-tags and the full version list, which is
# everything this check reads; the rest of the full document is per-version
# metadata nothing here looks at.
NPM_ABBREVIATED = "application/vnd.npm.install-v1+json"


def _urlopen_json(url):
    req = urllib.request.Request(url, headers={"Accept": NPM_ABBREVIATED})
    with urllib.request.urlopen(req, timeout=60) as resp:
        return json.load(resp)


def fetch_packument(url, fetcher=_urlopen_json):
    """The raw npm packument for @anthropic-ai/claude-code.

    Any failure - HTTP, timeout, JSON decode - raises FetchError. This never
    returns a partial or empty dict on failure: a currency check that
    silently stops checking is worse than no currency check.
    """
    try:
        return fetcher(url)
    except FetchError:
        raise
    except Exception as e:
        raise FetchError(f"could not read packument from {url}: {e}") from e


def parse_versions(doc):
    """packument -> (versions oldest-first, latest).

    Drops anything that is not exactly X.Y.Z - prereleases are not a
    yardstick this pin could ever legitimately hold. A missing or empty
    `versions` key maps to ([], None) outright - there is no yardstick to
    report `latest` against either. A missing `dist-tags` key with `versions`
    present maps to (versions, None); evaluate() reports both as problems.
    """
    versions_map = doc.get("versions") or {}
    if not versions_map:
        return [], None
    versions = sorted(
        (v for v in versions_map if SEMVER_RE.match(v)),
        key=lambda v: tuple(int(p) for p in v.split(".")),
    )
    latest = (doc.get("dist-tags") or {}).get("latest")
    return versions, latest


def read_pin(root):
    """The single ARG CLAUDE_CODE_VERSION=<value> match in <root>/Dockerfile.

    Zero or several matches, or an unreadable Dockerfile, all map to None.
    """
    try:
        text = (Path(root) / "Dockerfile").read_text(encoding="utf-8")
    except OSError:
        return None
    found = PIN_RE.findall(text)
    return found[0] if len(found) == 1 else None


def evaluate(pin, versions, latest, max_lag=MAX_LAG):
    """pin + published versions + dist-tags.latest -> list of problems."""
    problems = []

    if not versions:
        problems.append(
            f"{PACKUMENT_URL} published no releases at all; the currency check "
            "has nothing to compare against, which is a broken check, not a "
            "current pin."
        )
    if latest is None:
        problems.append(
            f"{PACKUMENT_URL} carries no dist-tags.latest, so there is no "
            "target to measure the pin against."
        )
    elif not SEMVER_RE.match(latest):
        # Distinct from the inconsistent-registry case below: a prerelease is
        # absent from `versions` only because parse_versions drops it, so
        # reporting "names no published release" would send the reader to npm
        # about a registry that is behaving perfectly.
        problems.append(
            f"{PACKUMENT_URL} dist-tags.latest={latest} is not a plain X.Y.Z "
            f"release. refresh-claude-code.yml refuses to pin the fleet to a "
            f"prerelease, so the pin is frozen until latest moves back to one."
        )
    elif versions and latest not in versions:
        problems.append(
            f"{PACKUMENT_URL} dist-tags.latest={latest} names no published "
            f"{PACKAGE} release; the registry response is inconsistent."
        )

    if pin is None:
        problems.append(
            "Dockerfile carries no single readable ARG CLAUDE_CODE_VERSION=, "
            "so currency could not be checked at all."
        )
        return problems

    if not versions or latest is None or latest not in versions:
        # The yardstick itself is broken; already reported above. A lag
        # computation against it would be meaningless.
        return problems

    if pin not in versions:
        problems.append(
            f"Dockerfile pins CLAUDE_CODE_VERSION={pin} which names no "
            f"published {PACKAGE} release. The image build would fail outright."
        )
        return problems

    lag = versions.index(latest) - versions.index(pin)
    if lag > max_lag:
        problems.append(
            f"Dockerfile pins CLAUDE_CODE_VERSION={pin}, {lag} published "
            f"releases behind dist-tags.latest={latest}. Check "
            f".github/workflows/refresh-claude-code.yml and the open "
            f"deps/claude-code PR FIRST: a lag over {max_lag} means the daily "
            f"bump cron stopped landing, not that a bump is in flight."
        )
    return problems


def main(argv):
    root = argv[0] if argv else str(Path(__file__).resolve().parents[2])
    try:
        doc = fetch_packument(PACKUMENT_URL)
    except FetchError as e:
        sys.stderr.write(
            f"::error::claude-code currency check could not fetch the packument: {e}\n"
        )
        return 1

    versions, latest = parse_versions(doc)
    pin = read_pin(root)
    problems = evaluate(pin, versions, latest)
    if problems:
        sys.stderr.write(
            "::error::CLAUDE_CODE_VERSION currency check failed; the pinned "
            "image may be running stale claude-code.\n"
        )
        for problem in problems:
            sys.stderr.write(f"  - {problem}\n")
        return 1
    print(f"CLAUDE_CODE_VERSION={pin} is current with {PACKAGE} latest={latest}.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
