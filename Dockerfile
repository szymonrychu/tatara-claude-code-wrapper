# syntax=docker/dockerfile:1.7

# Must satisfy go.mod's `go` directive (and match .mise.toml's go pin): the
# builder sets GOTOOLCHAIN=local, so a builder older than go.mod fails
# `go mod download` outright rather than fetching a newer toolchain.
ARG GO_VERSION=1.26.3
ARG NODE_VERSION=22
# Pinned for reproducible builds. Bumped by .github/workflows/refresh-claude-code.yml
# (daily npm check -> semver:patch auto-merge PR -> release rebuilds this layer).
ARG CLAUDE_CODE_VERSION=2.1.201
# v1.7.0 is the first cli tag reporting ContractVersion=3; an older one is
# refused by the operator at pod-ready. Bumped by the cli->wrapper cd-release.
ARG TATARA_CLI_VERSION=v2.1.0
# Skills plugin ref the wrapper boot-clones at runtime. Pinned to a semver tag so
# the skills->wrapper cd-release bump can rewrite this line (mirrors
# TATARA_CLI_VERSION). The Go default (cmd/wrapper/config.go) stays "main" for
# local dev; in the image this ENV pins it.
ARG TATARA_SKILLS_REF=v2.0.0
# renovate: repository=jdx/mise
ARG MISE_VERSION=v2026.6.3

# Stage 1: build the Go binaries (cached independently of the claude layer).
FROM golang:${GO_VERSION}-alpine AS go-build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X github.com/szymonrychu/tatara-claude-code-wrapper/internal/version.Version=${VERSION} -X github.com/szymonrychu/tatara-claude-code-wrapper/internal/version.Commit=${COMMIT} -X github.com/szymonrychu/tatara-claude-code-wrapper/internal/version.Date=${DATE}" \
      -o /out/wrapper ./cmd/wrapper && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/cc-stop-hook ./cmd/cc-stop-hook

# Stage 2: pull the tatara-cli binary at a pinned version.
FROM harbor.szymonrichert.pl/containers/tatara-cli:${TATARA_CLI_VERSION} AS tatara-cli

# Stage 3: guard -- verify the baked cli still advertises the tools the wrapper relies on.
# This stage runs the live-oracle tests in ./internal/bootstrap with
# /usr/local/bin/tatara from the tatara-cli stage on PATH.  The image build FAILS if:
#   - the pinned cli dropped submit_outcome / scm_read / issue_write / mr_write /
#     task_get / project_get / task_context / task_note / report_internal_issue
#     (TestTataraMCP_AdvertisesScmProjectTools), or
#   - any MCP tool name this repo puts in agent-facing prompt text -- today the
#     global CLAUDE.md this image injects into every pod -- is no longer advertised
#     (TestAgentPromptToolNamesAreAdvertised, #136). The names are discovered by
#     scanning the repo's own prompt constants, not read from a list someone must
#     remember to extend.
# TATARA_MCP_GUARD=required turns "no tatara binary" from a skip into a hard failure,
# so this stage cannot go green by quietly running nothing.
FROM golang:${GO_VERSION}-alpine AS test-guard
RUN apk add --no-cache git ca-certificates
COPY --from=tatara-cli /usr/local/bin/tatara /usr/local/bin/tatara
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./internal/promptguard -count=1
RUN TATARA_MCP_GUARD=required go test ./internal/bootstrap \
      -run 'TestTataraMCP_AdvertisesScmProjectTools|TestAgentPromptToolNamesAreAdvertised' -count=1 -v

# Stage 3b: a go toolchain used ONLY as a bind-mount source for the runtime
# stage's cgo/-race guard. Debian-based on purpose: the go-build stage above is
# alpine, and its go binary is linked against musl, so it cannot run in the
# bookworm-slim runtime. Nothing from this stage is copied into the image.
FROM golang:${GO_VERSION}-bookworm AS go-toolchain

# Stage 4: runtime -- node + claude in their own layer for trivial bumps.
FROM node:${NODE_VERSION}-bookworm-slim
# build-essential is NOT optional here (#145). Go's race detector requires cgo,
# and `go test ./... -race -count=1` is the canonical `make test` target in 6 of
# the project's 10 repos AND the pre-push hook. Without a C compiler Go forces
# CGO_ENABLED=0 and every -race run dies on
# `go: -race requires cgo; enable cgo by setting CGO_ENABLED=1`, so review pods
# silently approved PRs against a weaker suite than the repo contract specifies
# (29 distinct Tasks in 7 days), and tatara-memory-repo-ingester -- which depends
# on the cgo-only github.com/smacker/go-tree-sitter -- could not be BUILT at all.
# `git log -S gcc -- Dockerfile` was empty: the toolchain had never been here.
# build-essential rather than bare gcc: g++ is needed by tree-sitter grammars
# that ship a C++ external scanner, and make is what `make test` itself runs.
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl build-essential \
    && rm -rf /var/lib/apt/lists/*
# claude lives in its OWN layer: bumping CLAUDE_CODE_VERSION rebuilds only this.
ARG CLAUDE_CODE_VERSION
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && npm cache clean --force

COPY --from=tatara-cli /usr/local/bin/tatara /usr/local/bin/tatara
COPY --from=go-build /out/wrapper /usr/local/bin/wrapper
COPY --from=go-build /out/cc-stop-hook /usr/local/bin/cc-stop-hook

# Pin the skills ref the wrapper boot-clones (re-declare the global ARG to bring
# it into this stage's scope, then bake it as the runtime default ENV).
ARG TATARA_SKILLS_REF
ENV TATARA_SKILLS_REF=${TATARA_SKILLS_REF}

# non-root, writable HOME + workspace + skills clone dir (boot-cloned at runtime)
RUN useradd -m -u 10001 agent && mkdir -p /workspace /etc/wrapper && chown -R agent:agent /workspace /etc/wrapper

USER agent
ENV HOME=/home/agent HOME_DIR=/home/agent WORKSPACE=/workspace

# mise: per-user tool-version manager for the agent (matches the infra builder
# pattern). Installed as `agent` so it lands in /home/agent/.local; never as root.
# Each cloned repo pins its build tools in a root .mise.toml; the agent runs
# `mise install` per repo. Python + pre-commit are baked GLOBALLY here: the
# wrapper runs `pre-commit install` in every cloned repo (bootstrap hooks), so
# pre-commit must be on PATH even when a repo does not pin it in its own
# .mise.toml (e.g. tatara-agent-skills, tatara-documentation). pre-commit is a
# Python app and the node:bookworm-slim base has no python3, so a global python
# is baked with it (its hooks -- end-of-file-fixer, yamllint -- are Python too).
# python.github_attestations is disabled below: python-build-standalone builds
# before ~late-2024 (e.g. the 3.12.4 some repos pin) ship no GitHub attestations
# and mise >= 2026.6 rejects them by default; the download checksum still applies.
ARG MISE_VERSION
ENV MISE_VERSION=${MISE_VERSION}
# PYTHON_VERSION is pinned EXACT, not to the 3.13 minor. A floating minor
# resolves to whatever CPython released most recently, and mise only has a
# precompiled python-build-standalone build once that project publishes one -
# a lag of hours to days. In the gap mise silently falls back to compiling from
# source. That used to die immediately on
# `configure: error: no acceptable C compiler found in $PATH` (which is what
# broke the v2.0.0 release); now that build-essential is installed for cgo
# (#145) it would instead SUCCEED, adding many minutes to every image build for
# a python nobody asked to be built. python.compile=false below is what keeps a
# future miss an immediate, self-describing failure either way, and it matters
# more now than it did, not less.
# renovate: repository=python/cpython
ARG PYTHON_VERSION=3.13.15
# renovate: repository=pre-commit/pre-commit
ARG PRECOMMIT_VERSION=4.6.0
RUN curl https://mise.run | sh \
    && /home/agent/.local/bin/mise --version \
    && /home/agent/.local/bin/mise settings set plugin_autoupdate_last_check_duration "0" \
    && /home/agent/.local/bin/mise settings set not_found_auto_install "true" \
    && /home/agent/.local/bin/mise settings set auto_install "true" \
    && /home/agent/.local/bin/mise settings set task_run_auto_install "true" \
    && /home/agent/.local/bin/mise settings set experimental "true" \
    && /home/agent/.local/bin/mise settings set trusted_config_paths "/workspace" \
    && /home/agent/.local/bin/mise settings set python.github_attestations "false" \
    && /home/agent/.local/bin/mise settings set python.compile "false" \
    && /home/agent/.local/bin/mise use -g "python@${PYTHON_VERSION}" \
    && /home/agent/.local/bin/mise exec -- python3 --version \
    && /home/agent/.local/bin/mise use -g "pre-commit@${PRECOMMIT_VERSION}" \
    && /home/agent/.local/bin/mise exec -- pre-commit --version \
    && printf '%s\n' \
        'export PATH="$HOME/.local/bin:$PATH"' \
        'eval "$("$HOME/.local/bin/mise" activate bash)"' \
        >> /home/agent/.bash_profile

# mise binary + shims on PATH so the wrapper-spawned claude process and its
# non-interactive Bash tool calls resolve mise-managed tools. BASH_ENV covers
# login-style shells that need full `mise activate` (env + `mise exec`).
ENV PATH="/home/agent/.local/bin:/home/agent/.local/share/mise/shims:${PATH}"
ENV BASH_ENV="/home/agent/.bash_profile"
# __MISE_EXE must be a container ENV, not something only `mise activate` exports
# (#145). The activation above defines a `mise` shell FUNCTION whose last line
# is `command "$__MISE_EXE" "$command" "$@"`. In the agent's top-level Bash-tool
# shell the function survives but the export does not, so every `mise ...` call
# became `command "" ls`, landed in mise's own command_not_found_handle with an
# empty $1 and printed `bash: command not found: ` at rc 127 -- a message naming
# no tool, which is why agents concluded there was no mise fallback at all. The
# real binary was on PATH and working the whole time; the broken function
# shadowed it. Baking the value here makes the function correct in every shell
# context, whatever route the function itself arrived by.
ENV __MISE_EXE="/home/agent/.local/bin/mise"

# Guard 1 (#145): __MISE_EXE must come from the image ENV. `sh` does not read
# BASH_ENV and so never runs `mise activate`, which makes this fail if the value
# is ever demoted back to an activation-only export.
RUN sh -c 'test -n "$__MISE_EXE" && test -x "$__MISE_EXE"' \
    && bash -c 'mise --version' \
    && bash -lc 'mise --version'

# Guard 2 (#145): prove -race actually works in THIS image, on a cgo-enabled
# package, as the agent user. The go toolchain is bind-mounted for the check
# only -- it adds no layer and is not shipped (agents get go from each repo's
# .mise.toml, as before). This runs on every runtime build, unlike the
# test-guard stage above, which is outside the runtime DAG and only reached by
# .github/ci/build.sh's explicit --opt target=test-guard.
RUN --mount=from=go-toolchain,source=/usr/local/go,target=/opt/go <<'GUARD'
set -eu
d="$(mktemp -d)"
trap 'rm -rf "$d"' EXIT
cd "$d"
printf 'module cgoguard\n\ngo 1.24\n' > go.mod
cat > cgoguard.go <<'SRC'
package cgoguard

// #include <stdlib.h>
import "C"

// Answer round-trips through libc so the package cannot build without a working
// C toolchain, which is the whole point of the guard.
func Answer() int { return int(C.abs(C.int(-42))) }
SRC
cat > cgoguard_test.go <<'SRC'
package cgoguard

import "testing"

func TestAnswer(t *testing.T) {
	if got := Answer(); got != 42 {
		t.Fatalf("Answer() = %d, want 42", got)
	}
}
SRC
export PATH="/opt/go/bin:$PATH" GOTOOLCHAIN=local CGO_ENABLED=1 GOFLAGS= GOCACHE="$d/gocache" GOMODCACHE="$d/gomod" GOPATH="$d/gopath"
cc --version >/dev/null
go test -race -count=1 ./...
GUARD

WORKDIR /workspace
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/wrapper"]
