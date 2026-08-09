# Contract Version 4 (#521) - wrapper companion plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Master plan:** `tatara-operator/docs/superpowers/plans/2026-08-07-521-lifecycle-and-agent-merge.md`. This repo's change is MR5 of six. Read the master plan's "Merge choreography" section before merging anything here.

**Goal:** Bump `agent.ContractVersion` 3 -> 4 so the operator accepts this image after `tatara-cli` deleted the `clarify` `submit_outcome` profile and merged its decisions into implement's action enum.

**Architecture:** One constant and one pin test. This wrapper does not parse or validate outcome payloads - it hosts the `claude` PTY and serves `GET /v1/session`, from which the operator reads `contractVersion` at pod-ready via `agent.AssertContractVersion` (`tatara-operator/internal/agent/session.go:115`, called from `internal/controller/podwatch.go:181`). A mismatch fails the Task BEFORE turn 0, loud, with zero tokens burned.

**Tech Stack:** Go (see `.mise.toml`), testify, helm unittest.

## Global Constraints

- KISS. Never introduce tech debt.
- No em dashes, smart quotes, arrows, or decorative Unicode.
- semver push-CD: this PR carries a literal `semver:major` label.
- Agents never merge PRs.
- Build and deploy from `main` only, never a worktree.

---

## The design doc's "no test edits needed" claim is FALSE

The reconciled design says:

> `tatara-claude-code-wrapper/internal/version/version.go:20` (its tests compare symbolically, so NO wrapper test edits needed - the one repo where the bump is a one-line change).

Audited against `origin/main` on 2026-08-07. Three of the four sites are symbolic; one is not.

| site | form | safe under a bump? |
|---|---|---|
| `internal/session/session_test.go:87` | `require.Equal(t, version.ContractVersion, m.Snapshot().ContractVersion)` | YES |
| `internal/httpapi/messages_test.go:48` | `session.Snapshot{State: session.Ready, ContractVersion: version.ContractVersion}` | YES |
| `internal/httpapi/messages_test.go:388` | `require.Equal(t, float64(version.ContractVersion), got["contractVersion"], ...)` | YES - and note this one WAS a hardcoded `float64(2)` two commits before `origin/main`; it was fixed in the 2 -> 3 bump |
| **`internal/version/version_test.go:5-8`** | `func TestContractVersionIsThree` / `if ContractVersion != 3` | **NO. Hardcoded name AND literal.** |

So MR5 is a two-file change. It is still the smallest MR in the landing, and the claim was worth checking rather than trusting: the pin test exists precisely so a bump cannot be silent, and it would have failed CI rather than shipped broken - but the plan would have said "one line" and been wrong.

No non-test site writes the value as a literal. `internal/session/session.go:109-110` declares the JSON field and `:1258` populates it from `version.ContractVersion`; nothing else produces it.

---

## Task 1: the bump

**Files:**
- Modify: `internal/version/version.go:20`
- Modify: `internal/version/version_test.go:5-8`
- Modify: `MEMORY.md`

**Interfaces:**
- Produces: `version.ContractVersion == 4`, served on `GET /v1/session` as `contractVersion`.
- Consumes: nothing. This repo is the last of the three to bump and gates nothing except MR6 + MR7's maintenance slot.

- [ ] **Step 1: Write the failing test**

Replace the pin test in `internal/version/version_test.go`:

```go
func TestContractVersionIsFour(t *testing.T) {
	if ContractVersion != 4 {
		t.Fatalf("ContractVersion = %d, want 4 (must match tatara-operator and tatara-cli)", ContractVersion)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `mise exec -- go test ./internal/version/ -run TestContractVersion -v`

Expected: FAIL with `ContractVersion = 3, want 4`.

- [ ] **Step 3: Write the minimal implementation**

`internal/version/version.go:20`:

```go
const ContractVersion = 4
```

Extend the existing doc comment (lines 13-19) with one line naming the cause, so the next reader does not have to find the issue:

```go
// 3 -> 4 (#521): tatara-cli deleted the clarify submit_outcome profile and
// merged its implement/close/discuss decisions into implement's action enum as
// approved/discuss/rejected, alongside the new approvingMaintainer and
// planNoteId gate fields.
```

- [ ] **Step 4: Run the tests and PROVE the other three sites really were symbolic**

Run:

```bash
mise exec -- make test
grep -rn "ContractVersion" --include="*_test.go" .
```

Expected: `make test` green with no further edits, and every remaining hit reads `version.ContractVersion` rather than a numeric literal. If any test fails, the symbolic claim was wrong for that site too: fix it and record the site in `MEMORY.md` so the next bump's audit starts from the truth.

- [ ] **Step 5: Run the full verification**

Run:

```bash
mise exec -- make test
mise exec -- make lint
mise exec -- make chart-test
mise exec -- pre-commit run --all-files
```

Expected: all four green.

- [ ] **Step 6: Commit**

```bash
git add internal/version/ MEMORY.md
git commit -m "feat!: contract version 4

tatara-cli deleted the clarify submit_outcome profile and merged its decisions
into implement's action enum (#521). The operator asserts this value from
GET /v1/session at pod-ready, BEFORE turn 0, so an operator on contract 3 must
refuse this image."
```

`MEMORY.md` line to add:

```
2026-08-XX: ContractVersion 3 -> 4 (#521), in lockstep with tatara-operator and tatara-cli. The #521 design doc claimed this repo needed NO test edit because "its tests compare symbolically" - that was wrong: internal/version/version_test.go's TestContractVersionIsThree hardcodes both the name and the literal. The other three sites (session_test.go:87, messages_test.go:48 and :388) genuinely are symbolic, and :388 only became so during the 2 -> 3 bump. Audit all four on the next bump; do not trust the claim.
```

---

## Merge position

This is **step 4 of 6** in the master plan's choreography, after `tatara-cli` (MR4) and `tatara-agent-skills` (MR3), before `tatara-operator` (MR6) and `tatara-helmfile` (MR7).

Merging this publishes a new wrapper image to Harbor. **It does not deploy it:** the three Project CRs in `tatara-helmfile` pin `spec.agent.image` to `v1.3.5`, and MR7 is what moves that pin. Between MR6 landing and MR7 applying, every agent pod fails `AssertContractVersion` at pod-ready - loud, pre-work, zero tokens. That window is accepted and documented in the master plan.

**Semver: `major`.** The operator refuses this image unless it is also on contract 4. That is breaking by the contract's own definition, and by the precedent of the 2 -> 3 bump.
