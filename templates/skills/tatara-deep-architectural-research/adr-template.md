# ADR Template and Technology Radar

Reference doc for the `tatara-deep-architectural-research` skill.
The SKILL.md SYNTHESIZE step produces an ADR following the template below.
The Radar convention governs which options are admissible.

---

## ADR Template

```
# ADR <number>: <short imperative title>

Date: YYYY-MM-DD
Status: proposed | accepted | superseded-by ADR-N

## Context

<The problem statement. What hurts, why it matters to the platform or the
repo goal. Evidence: file:line references and concrete graph findings.
Keep it to 3-5 sentences; use bullet points only for evidence lists.>

Evidence:
- <file:line or graph finding>
- <file:line or graph finding>

## Options

### Option A: <name>

<Description. How it works. In Phase 1, citations are memory-graph/on-disk
file:line + MEMORY.md entries. In Phase 2+, include arXiv/OpenAlex paper
references. Include the Technology Radar ring (adopt/trial/assess/hold).>

Tradeoffs:
- Pro: <...>
- Con: <...>
Radar ring: <adopt | trial | assess | hold>

### Option B: <name>

...

### Option C: <name>

...

## Decision

Recommended: Option <X>, because <one-line rationale>.

## Consequences

What this unblocks: <...>
What it costs (effort / risk): <...>
Behavior-preservation gate: <the test/check that proves no regression>

## Fitness Function

<The deterministic CI check that encodes this decision as an invariant
so it cannot silently regress. Examples:
- Import-graph check: `go list -deps ./... | grep go-github` fails if
  called from outside the adapter package (ast-grep enforces the seam).
- Chart-size check: `helm template | wc -l` fails above a threshold.
- Commit-hygiene check: a refactor PR touches no behavior tests.
The fitness function is the audit trail that the ADR was adopted.>

## Open Questions

<Explicitly list unresolved questions here - unlike the issue body, the
ADR allows open questions. Required carry-in for Phase-1 runs:
- field survey: external sources (arXiv, OpenAlex, web) not yet available;
  Phase 2 will activate the SURVEY step with citations.
Add any other open questions relevant to this decision.>
```

---

## Technology Radar Convention

Four rings, applied to each option in the ADR and to any technique the
skill proposes for adoption:

**adopt** - Proven in tatara's context or directly endorsed by verified
literature (`[OK high]`). Use without reservation. No special approval gate
beyond the normal MR flow.

**trial** - Sound approach, limited tatara evidence. Use for one bounded
deliverable and measure the outcome before widening. Needs explicit rationale
in the ADR.

**assess** - Plausible but unverified (`[UNVERIFIED]` in the design doc).
Understand it, build a spike, do not ship production code based on it yet.
Needs a fitness function before the trial ring.

**hold** - Explicitly prohibited for new work. No new direct vendor-SDK
imports in core outside a designated adapter package (the strangler-fig seam
from design section 4(c)). Anything needing weight updates or live self-patch.

The **hold** ring directly enforces the ports-and-adapters strangler boundary:
once a fitness function (import-graph CI check) encodes the seam, every new
direct `go-github` or `gitlab` import in operator/wrapper core trips CI and
blocks the merge. The hold ring is the policy; the fitness function is the
enforcement.

### Worked example radar entry

| Technique | Ring | Rationale |
|---|---|---|
| `SCMProvider` port + `GitHubAdapter` strangler | adopt | `[OK high]` Fowler Strangler Fig + Ports-and-Adapters; behavior-preserving; each step reversible; maps cleanly to tatara's sonnet-implements/opus-merges rule |
| Serena code-intelligence MCP | adopt | Zero egress, no API key; IDE-grade symbol nav for strangler call-site rewrites; safe on all pod profiles |
| arXiv + OpenAlex academic MCP | trial | Free, key-optional; field-survey capability for Phase 2; needs egress enabled first |
| Co-scientist tournament admission | assess | Pure orchestration over existing stack; `[UNVERIFIED]` on tatara; spike needed |
| Direct `go-github` import in operator core | hold | Breaks the SCMProvider seam; CI fitness function enforces this ring |
