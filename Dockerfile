# ---- Go build stage ----
FROM docker.io/library/golang:1.26.1-alpine3.23.3 AS builder

RUN apk add --no-cache git chafa

WORKDIR /src
COPY go.mod go.sum* ./
COPY src/ ./src/
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN go mod tidy

# Generate banner art from logo.png (produces banner_art_gen.go with escaped ANSI).
RUN go generate ./src/output/...

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 go build -tags banner_art \
      -ldflags "-s -w \
        -X github.com/PrPlanIT/StageFreight/src/version.Version=${VERSION} \
        -X github.com/PrPlanIT/StageFreight/src/version.Commit=${COMMIT} \
        -X github.com/PrPlanIT/StageFreight/src/version.BuildDate=${BUILD_DATE}" \
      -o /out/stagefreight ./src/cli

# ---- Runtime image ----
FROM docker.io/library/alpine:3.23.3

LABEL maintainer="PrPlanIT <precisionplanit@gmail.com>" \
      org.opencontainers.image.title="StageFreight" \
      org.opencontainers.image.description="Declarative CI/CD automation CLI — detect, build, scan, and release container images from a single manifest." \
      org.opencontainers.image.source="https://github.com/PrPlanIT/StageFreight" \
      org.opencontainers.image.url="https://hub.docker.com/r/prplanit/stagefreight" \
      org.opencontainers.image.documentation="https://github.com/PrPlanIT/StageFreight#readme" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.vendor="PrPlanIT"

# Runtime dependencies — only what stagefreight actually shells out to.
RUN apk add --no-cache \
      chafa \
      docker-cli \
      git \
      tree

# UTF-8 locale for chafa Unicode block characters in CI logs.
ENV LANG=C.UTF-8

# Pinned tool versions — bump these for updates.
ENV BUILDX_VERSION=v0.32.0 \
    TRIVY_VERSION=0.69.3 \
    SYFT_VERSION=1.42.1 \
    GRYPE_VERSION=0.109.0 \
    OSV_SCANNER_VERSION=2.3.3

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

ENV COSIGN_VERSION=3.0.5

# Install cosign (container signing and verification)
RUN wget -qO /usr/local/bin/cosign \
      "https://github.com/sigstore/cosign/releases/download/v${COSIGN_VERSION}/cosign-linux-amd64" && \
    chmod +x /usr/local/bin/cosign

# Copy the Go binary from builder stage.
COPY --from=builder /out/stagefreight /usr/local/bin/stagefreight

CMD ["/bin/sh"]
