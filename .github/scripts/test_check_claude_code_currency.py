import importlib.util
from pathlib import Path

import pytest

HERE = Path(__file__).parent
REPO_ROOT = HERE.parents[1]
SCRIPT = HERE / "check_claude_code_currency.py"
_spec = importlib.util.spec_from_file_location("check_claude_code_currency", SCRIPT)
currency = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(currency)


def _doc(versions=None, latest=None, stable=None):
    doc = {}
    if versions is not None:
        doc["versions"] = {v: {} for v in versions}
    dist_tags = {}
    if latest is not None:
        dist_tags["latest"] = latest
    if stable is not None:
        dist_tags["stable"] = stable
    if versions is not None or latest is not None or stable is not None:
        doc["dist-tags"] = dist_tags
    return doc


# --- version parsing ---


def test_parse_versions_orders_numerically_not_lexically():
    doc = _doc(versions=["2.1.9", "2.1.10", "2.10.0", "2.1.2"], latest="2.10.0")
    versions, latest = currency.parse_versions(doc)
    assert versions == ["2.1.2", "2.1.9", "2.1.10", "2.10.0"]
    assert latest == "2.10.0"


def test_parse_versions_drops_prereleases():
    doc = _doc(versions=["2.1.0", "2.1.1-beta.0", "2.1.1", "2.2.0-rc.1"], latest="2.1.1")
    versions, _ = currency.parse_versions(doc)
    assert versions == ["2.1.0", "2.1.1"]


def test_parse_versions_missing_versions_key_returns_empty_and_none():
    versions, latest = currency.parse_versions({"dist-tags": {"latest": "2.1.1"}})
    assert versions == []
    assert latest is None


def test_parse_versions_missing_dist_tags_returns_latest_none():
    versions, latest = currency.parse_versions({"versions": {"2.1.0": {}, "2.1.1": {}}})
    assert versions == ["2.1.0", "2.1.1"]
    assert latest is None


# --- read_pin ---


def test_read_pin_reads_the_equals_form(tmp_path):
    (tmp_path / "Dockerfile").write_text(
        "ARG CLAUDE_CODE_VERSION=2.1.201\n"
        "RUN echo hi\n"
    )
    assert currency.read_pin(tmp_path) == "2.1.201"


def test_read_pin_ignores_the_bare_redeclaration(tmp_path):
    """The real Dockerfile shape: one `=` pin plus a bare re-declaration in a later layer."""
    (tmp_path / "Dockerfile").write_text(
        "ARG CLAUDE_CODE_VERSION=2.1.201\n"
        "FROM base AS claude\n"
        "ARG CLAUDE_CODE_VERSION\n"
        "RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}\n"
    )
    assert currency.read_pin(tmp_path) == "2.1.201"


def test_read_pin_two_different_equals_pins_is_none(tmp_path):
    (tmp_path / "Dockerfile").write_text(
        "ARG CLAUDE_CODE_VERSION=2.1.201\n"
        "ARG CLAUDE_CODE_VERSION=2.1.202\n"
    )
    assert currency.read_pin(tmp_path) is None


def test_read_pin_missing_file_is_none(tmp_path):
    assert currency.read_pin(tmp_path) is None


def test_read_pin_against_the_real_repo_dockerfile():
    pin = currency.read_pin(REPO_ROOT)
    assert pin is not None
    assert len(pin.split(".")) == 3
    assert all(p.isdigit() for p in pin.split("."))


# --- lag rule ---


VERSIONS = ["2.1.238", "2.1.239", "2.1.240", "2.1.241"]


def test_lag_zero_is_clean():
    assert currency.evaluate("2.1.241", VERSIONS, "2.1.241") == []


def test_lag_one_is_clean():
    assert currency.evaluate("2.1.240", VERSIONS, "2.1.241") == []


def test_lag_two_is_clean():
    assert currency.evaluate("2.1.239", VERSIONS, "2.1.241") == []


def test_lag_three_is_reported():
    problems = currency.evaluate("2.1.238", VERSIONS, "2.1.241")
    assert len(problems) == 1
    assert "2.1.238" in problems[0]
    assert "3" in problems[0]
    assert "2.1.241" in problems[0]


# --- anchoring on dist-tags.latest, not the newest published version ---


def test_anchors_on_latest_not_the_newest_published_version():
    """A packument whose newest published version is above dist-tags.latest.

    stable=2.1.231, latest=2.1.241, next=2.1.241 is the measured shape: `next`
    can publish ahead of `latest`. The pin must be judged against `latest`
    only; the excess above it is not lag.
    """
    versions = ["2.1.238", "2.1.239", "2.1.240", "2.1.241", "2.1.242", "2.1.243"]
    # pin is exactly at latest even though newer versions exist beyond it.
    assert currency.evaluate("2.1.241", versions, "2.1.241") == []
    # pin is 2 behind latest even though it is 4 behind the newest published.
    assert currency.evaluate("2.1.239", versions, "2.1.241") == []


# --- the five non-lag problem branches ---


def test_no_versions_at_all_is_reported():
    problems = currency.evaluate("2.1.241", [], "2.1.241")
    assert len(problems) == 1
    assert "no releases" in problems[0] or "no published" in problems[0]


def test_missing_latest_is_reported():
    problems = currency.evaluate("2.1.241", VERSIONS, None)
    assert len(problems) == 1
    assert "dist-tags.latest" in problems[0]


def test_latest_not_in_versions_is_reported():
    problems = currency.evaluate("2.1.241", VERSIONS, "9.9.9")
    assert len(problems) == 1
    assert "9.9.9" in problems[0]


def test_pin_none_is_reported():
    problems = currency.evaluate(None, VERSIONS, "2.1.241")
    assert len(problems) == 1
    assert "Dockerfile" in problems[0]
    assert "ARG CLAUDE_CODE_VERSION=" in problems[0]


def test_pin_not_in_versions_is_reported():
    problems = currency.evaluate("9.9.9", VERSIONS, "2.1.241")
    assert len(problems) == 1
    assert "9.9.9" in problems[0]


def test_rules_one_through_three_are_reported_even_with_unreadable_pin():
    """The yardstick problems fire regardless of whether the pin itself is readable."""
    assert currency.evaluate(None, [], None) != []
    problems = currency.evaluate(None, [], None)
    assert any("no releases" in p or "no published" in p for p in problems)


# --- fail-closed ---


def _failing_fetcher(url):
    raise OSError("network is unreachable")


def test_fetch_packument_raises_rather_than_returning_partial_data():
    with pytest.raises(currency.FetchError):
        currency.fetch_packument(currency.PACKUMENT_URL, fetcher=_failing_fetcher)


def test_fetch_packument_returns_the_fetcher_output_verbatim():
    doc = {"versions": {"2.1.0": {}}, "dist-tags": {"latest": "2.1.0"}}
    assert currency.fetch_packument("x", fetcher=lambda url: doc) is doc


def test_main_returns_1_and_writes_error_on_fetch_failure(monkeypatch, capsys):
    monkeypatch.setattr(currency, "fetch_packument", lambda url, fetcher=None: (_ for _ in ()).throw(currency.FetchError("boom")))
    rc = currency.main([])
    captured = capsys.readouterr()
    assert rc == 1
    assert "::error::" in captured.err


def test_main_returns_1_and_writes_error_when_problems_found(tmp_path, monkeypatch):
    (tmp_path / "Dockerfile").write_text("ARG CLAUDE_CODE_VERSION=2.1.238\n")
    doc = {"versions": {v: {} for v in VERSIONS}, "dist-tags": {"latest": "2.1.241"}}
    monkeypatch.setattr(currency, "fetch_packument", lambda url, fetcher=None: doc)
    rc = currency.main([str(tmp_path)])
    assert rc == 1


def test_main_returns_0_when_clean(tmp_path, monkeypatch, capsys):
    (tmp_path / "Dockerfile").write_text("ARG CLAUDE_CODE_VERSION=2.1.241\n")
    doc = {"versions": {v: {} for v in VERSIONS}, "dist-tags": {"latest": "2.1.241"}}
    monkeypatch.setattr(currency, "fetch_packument", lambda url, fetcher=None: doc)
    rc = currency.main([str(tmp_path)])
    captured = capsys.readouterr()
    assert rc == 0
    assert "2.1.241" in captured.out


# --- the tolerance itself ---


def test_max_lag_is_two():
    """MAX_LAG must be 2, not 1 as in check_skills_currency.py.

    Measured: over the last 60 releases, the lag introduced in the 06:17Z to
    09:17Z window (bump cron to this check) was 0 on 49 of 50 days and 1 on
    the remaining one. It takes roughly 2-3 consecutive dead cron days to
    red. Lowering this must be a deliberate edit against a failing test, not
    an accidental copy from the skills check.
    """
    assert currency.MAX_LAG == 2


def test_a_prerelease_latest_is_not_reported_as_an_inconsistent_registry():
    """parse_versions drops a prerelease, so `latest not in versions` fires for
    a registry that is behaving perfectly. Reporting it as inconsistent sends
    the reader to npm instead of to the frozen pin.
    """
    problems = currency.evaluate("2.1.201", ["2.1.200", "2.1.201"], "2.2.0-rc.1")
    assert len(problems) == 1
    assert "not a plain X.Y.Z release" in problems[0]
    assert "inconsistent" not in problems[0]


def test_a_latest_absent_from_a_plain_version_list_is_still_inconsistent():
    """The other branch must survive: a plain X.Y.Z latest that is not in the
    published set really is a broken registry answer.
    """
    problems = currency.evaluate("2.1.201", ["2.1.200", "2.1.201"], "2.1.999")
    assert len(problems) == 1
    assert "inconsistent" in problems[0]
