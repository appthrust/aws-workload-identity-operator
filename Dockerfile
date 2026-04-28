# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /workspace
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /workspace/bin && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -a -o /workspace/bin/manager ./cmd/manager && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -a -o /workspace/bin/aws-remote-irsa-credential-process ./cmd/aws-remote-irsa-credential-process && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -a -o /workspace/bin/aws-irsa-sidecar ./cmd/aws-irsa-sidecar

FROM gcr.io/distroless/static:nonroot AS remote-irsa-tools
WORKDIR /
COPY --from=builder /workspace/bin/aws-remote-irsa-credential-process /plugins/aws-remote-irsa-credential-process
COPY --from=builder /workspace/bin/aws-irsa-sidecar /plugins/aws-irsa-sidecar
USER 65532:65532
ENTRYPOINT ["/plugins/aws-remote-irsa-credential-process"]

FROM gcr.io/distroless/static:nonroot AS aws-irsa-sidecar
WORKDIR /
COPY --from=builder /workspace/bin/aws-irsa-sidecar /aws-irsa-sidecar
USER 65532:65532
ENTRYPOINT ["/aws-irsa-sidecar"]

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/bin/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
