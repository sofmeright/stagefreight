# ---- Go build stage ----
FROM docker.io/library/golang:1.26.1-alpine3.23 AS builder

RUN apk add --no-cache git chafa

WORKDIR /src

# Module download — cached independently of source changes.
# Only re-runs when go.mod/go.sum change.
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Source copy — invalidates build layers but NOT module cache.
COPY src/ ./src/
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# Tidy with mount cache — uses cached modules, only adds missing ones.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod tidy

# Generate banner art from logo.png (produces banner_art_gen.go with escaped ANSI).
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go generate ./src/output/...

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Build with persistent Go build cache — only recompiles changed packages.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 go build -tags banner_art \
      -ldflags "-s -w \
        -X github.com/PrPlanIT/StageFreight/src/version.Version=${VERSION} \
        -X github.com/PrPlanIT/StageFreight/src/version.Commit=${COMMIT} \
        -X github.com/PrPlanIT/StageFreight/src/version.BuildDate=${BUILD_DATE}" \
      -o /out/stagefreight ./src/cli

# ---- Runtime image ----
FROM docker.io/library/alpine:3.23.3

LABEL maintainer="PrPlanIT <precisionplanit@gmail.com>" \
      org.opencontainers.image.title="StageFreight" \
      org.opencontainers.image.description="A declarative lifecycle runtime that governs Git as the source of truth, enforcing operator-defined intent across GitOps workflows, Kubernetes, Docker, and CI ecosystems." \
      org.opencontainers.image.source="https://github.com/PrPlanIT/StageFreight" \
      org.opencontainers.image.url="https://hub.docker.com/r/prplanit/stagefreight" \
      org.opencontainers.image.documentation="https://github.com/PrPlanIT/StageFreight#readme" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.vendor="PrPlanIT"

# Runtime dependencies — git is intentionally absent: the go-git pure-Go transport
# handles all repository operations natively (SSH via golang.org/x/crypto/ssh,
# HTTPS via net/http). No git binary, no openssh-client required.
RUN apk add --no-cache \
      chafa \
      tree

# UTF-8 locale for chafa Unicode block characters in CI logs.
ENV LANG=C.UTF-8

# Pinned tool versions — bump these for updates.
ENV BUILDX_VERSION=v0.33.0 \
    COSIGN_VERSION=3.0.6 \
    DOCKER_VERSION=29.3.1 \
    FLUX_VERSION=2.8.3 \
    GRYPE_VERSION=0.110.0 \
    KUBECTL_VERSION=1.34.2 \
    OSV_SCANNER_VERSION=2.3.5 \
    SYFT_VERSION=1.42.3 \
    TRIVY_VERSION=0.69.3

# Install docker CLI (static binary — Alpine apk lags behind security patches)
RUN wget -qO /tmp/docker.tgz \
      "https://download.docker.com/linux/static/stable/x86_64/docker-${DOCKER_VERSION}.tgz" && \
    tar -xzf /tmp/docker.tgz -C /tmp docker/docker && \
    mv /tmp/docker/docker /usr/local/bin/docker && \
    chmod +x /usr/local/bin/docker && \
    rm -rf /tmp/docker.tgz /tmp/docker

# Install docker buildx
RUN mkdir -p ~/.docker/cli-plugins && \
    wget -qO ~/.docker/cli-plugins/docker-buildx \
      "https://github.com/docker/buildx/releases/download/${BUILDX_VERSION}/buildx-${BUILDX_VERSION}.linux-amd64" && \
    chmod +x ~/.docker/cli-plugins/docker-buildx

# Install trivy (vulnerability scanner)
RUN wget -qO /tmp/trivy.tar.gz \
      "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/trivy_${TRIVY_VERSION}_Linux-64bit.tar.gz" && \
    tar -xzf /tmp/trivy.tar.gz -C /usr/local/bin trivy && \
    rm /tmp/trivy.tar.gz

# Install syft (SBOM generator)
RUN wget -qO /tmp/syft.tar.gz \
      "https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}/syft_${SYFT_VERSION}_linux_amd64.tar.gz" && \
    tar -xzf /tmp/syft.tar.gz -C /usr/local/bin syft && \
    rm /tmp/syft.tar.gz

# Install grype (vulnerability scanner — complements Trivy with Anchore's DB)
RUN wget -qO /tmp/grype.tar.gz \
      "https://github.com/anchore/grype/releases/download/v${GRYPE_VERSION}/grype_${GRYPE_VERSION}_linux_amd64.tar.gz" && \
    tar -xzf /tmp/grype.tar.gz -C /usr/local/bin grype && \
    rm /tmp/grype.tar.gz

# Install osv-scanner (source-level vulnerability scanner via OSV database)
RUN wget -qO /usr/local/bin/osv-scanner \
      "https://github.com/google/osv-scanner/releases/download/v${OSV_SCANNER_VERSION}/osv-scanner_linux_amd64" && \
    chmod +x /usr/local/bin/osv-scanner

# Install flux CLI (GitOps reconciliation)
RUN wget -qO /tmp/flux.tar.gz \
      "https://github.com/fluxcd/flux2/releases/download/v${FLUX_VERSION}/flux_${FLUX_VERSION}_linux_amd64.tar.gz" && \
    tar -xzf /tmp/flux.tar.gz -C /usr/local/bin flux && \
    rm /tmp/flux.tar.gz

# Install kubectl (Kubernetes API client)
RUN wget -qO /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/amd64/kubectl" && \
    chmod +x /usr/local/bin/kubectl

# Install cosign (container signing and verification)
RUN wget -qO /usr/local/bin/cosign \
      "https://github.com/sigstore/cosign/releases/download/v${COSIGN_VERSION}/cosign-linux-amd64" && \
    chmod +x /usr/local/bin/cosign

# StageFreight runtime paths.
# /stagefreight/cache — persistent (mount a volume here for cross-run reuse)
# /tmp/stagefreight   — ephemeral scratch (dies with the container)
RUN mkdir -p /stagefreight/cache /tmp/stagefreight

# Copy the Go binary from builder stage.
COPY --from=builder /out/stagefreight /usr/local/bin/stagefreight

CMD ["/bin/sh"]
