// Package version holds build-time version information populated via ldflags.
package version

// Version is the semantic version string, set at build time.
var Version = "dev"

// Commit is the git commit SHA, set at build time.
var Commit = "unknown"

// Date is the build timestamp in RFC3339 format, set at build time.
var Date = "unknown"

// ContractVersion is the cross-repo agent contract this image implements. The
// operator injects TATARA_CONTRACT_VERSION into the pod and asserts this value
// from GET /v1/session at pod-ready, BEFORE submitting turn-0. A mismatch means
// the agent image and the operator came from different release trains: every
// tool call the pod makes would 404, and it would burn its whole turn budget
// working around them, silently. Bump this in the same release that ships a
// breaking agent-facing change. Contract G.10.
const ContractVersion = 2
