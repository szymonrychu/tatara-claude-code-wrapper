# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.25
ARG NODE_VERSION=22
ARG CLAUDE_CODE_VERSION=latest
ARG TATARA_CLI_VERSION=0.6.0

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
# the pinned cli dropped propose_issue / review_verdict / pr_outcome / issue_outcome.
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
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# claude lives in its OWN layer: bumping CLAUDE_CODE_VERSION rebuilds only this.
ARG CLAUDE_CODE_VERSION
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && npm cache clean --force

COPY --from=tatara-cli /usr/local/bin/tatara /usr/local/bin/tatara
COPY --from=go-build /out/wrapper /usr/local/bin/wrapper
COPY --from=go-build /out/cc-stop-hook /usr/local/bin/cc-stop-hook
COPY templates/ /templates/

# non-root, writable HOME + workspace
RUN useradd -m -u 10001 agent && mkdir -p /workspace && chown -R agent:agent /workspace /templates
USER agent
ENV HOME=/home/agent HOME_DIR=/home/agent WORKSPACE=/workspace
WORKDIR /workspace
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/wrapper"]
