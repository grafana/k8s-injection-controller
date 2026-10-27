# Build the manager binary
FROM golang:1.26.10@sha256:672c944cfd07599b5f1bb179869270cdc94c6254e41db4d431259693cb286b82 AS builder
ARG TARGETOS
ARG TARGETARCH

# Version metadata stamped into the binary via -ldflags. Passed by the release
# workflow / `mise run docker-build`; defaults keep local `docker build` working.
ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown

WORKDIR /workspace

COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call mise run docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w \
      -X github.com/grafana/beyla-k8s-injector/internal/buildinfo.Version=${VERSION} \
      -X github.com/grafana/beyla-k8s-injector/internal/buildinfo.Revision=${REVISION} \
      -X github.com/grafana/beyla-k8s-injector/internal/buildinfo.Date=${BUILD_DATE}" \
    -o manager ./cmd

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
