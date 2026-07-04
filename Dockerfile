# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.25
ARG NODE_VERSION=22
ARG CLAUDE_CODE_VERSION=latest
ARG TATARA_CLI_VERSION=c8691d1
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
# This stage runs `go test ./internal/bootstrap -run TestTataraMCP_AdvertisesScmProjectTools`
# with /usr/local/bin/tatara from the tatara-cli stage on PATH.  The image build FAILS if
# the pinned cli dropped propose_issue / review_verdict / pr_outcome / issue_outcome / comment.
FROM golang:${GO_VERSION}-alpine AS test-guard
RUN apk add --no-cache git ca-certificates
COPY --from=tatara-cli /usr/local/bin/tatara /usr/local/bin/tatara
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./internal/bootstrap -run TestTataraMCP_AdvertisesScmProjectTools -count=1

# Stage 4: runtime -- node + claude in their own layer for trivial bumps.
FROM node:${NODE_VERSION}-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
# claude lives in its OWN layer: bumping CLAUDE_CODE_VERSION rebuilds only this.
ARG CLAUDE_CODE_VERSION
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && npm cache clean --force

COPY --from=tatara-cli /usr/local/bin/tatara /usr/local/bin/tatara
COPY --from=go-build /out/wrapper /usr/local/bin/wrapper
COPY --from=go-build /out/cc-stop-hook /usr/local/bin/cc-stop-hook

# non-root, writable HOME + workspace + skills clone dir (boot-cloned at runtime)
RUN useradd -m -u 10001 agent && mkdir -p /workspace /etc/wrapper && chown -R agent:agent /workspace /etc/wrapper

USER agent
ENV HOME=/home/agent HOME_DIR=/home/agent WORKSPACE=/workspace

# mise: per-user tool-version manager for the agent (matches the infra builder
# pattern). Installed as `agent` so it lands in /home/agent/.local; never as root.
# Each cloned repo pins its build tools in a root .mise.toml; the agent runs
# `mise install` per repo. Python is the one GLOBAL tool baked here: pre-commit
# (and its python hooks: end-of-file-fixer, yamllint, etc.) is a Python app and
# the node:bookworm-slim base has no python3 -- without a global python `mise use
# -g python`, `pre-commit install` fails with `/usr/bin/env: python3: not found`.
ARG MISE_VERSION
ENV MISE_VERSION=${MISE_VERSION}
# renovate: repository=python/cpython
ARG PYTHON_VERSION=3.13
RUN curl https://mise.run | sh \
    && /home/agent/.local/bin/mise --version \
    && /home/agent/.local/bin/mise settings set plugin_autoupdate_last_check_duration "0" \
    && /home/agent/.local/bin/mise settings set not_found_auto_install "true" \
    && /home/agent/.local/bin/mise settings set auto_install "true" \
    && /home/agent/.local/bin/mise settings set task_run_auto_install "true" \
    && /home/agent/.local/bin/mise settings set experimental "true" \
    && /home/agent/.local/bin/mise settings set trusted_config_paths "/workspace" \
    && /home/agent/.local/bin/mise use -g "python@${PYTHON_VERSION}" \
    && /home/agent/.local/bin/mise exec -- python3 --version \
    && printf '%s\n' \
        'export PATH="$HOME/.local/bin:$PATH"' \
        'eval "$("$HOME/.local/bin/mise" activate bash)"' \
        >> /home/agent/.bash_profile

# mise binary + shims on PATH so the wrapper-spawned claude process and its
# non-interactive Bash tool calls resolve mise-managed tools. BASH_ENV covers
# login-style shells that need full `mise activate` (env + `mise exec`).
ENV PATH="/home/agent/.local/bin:/home/agent/.local/share/mise/shims:${PATH}"
ENV BASH_ENV="/home/agent/.bash_profile"

WORKDIR /workspace
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/wrapper"]
