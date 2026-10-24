# Built from source because Go's x509 fixes are compiled in; upstream helm/kubectl release binaries lag Go patch releases.
FROM golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS builder

ARG HELM_VERSION=v4.3.1
ARG KUBECTL_VERSION=v1.37.1

ENV CGO_ENABLED=0

RUN GOBIN=/out go install -trimpath \
      -ldflags="-s -w \
        -X helm.sh/helm/v4/internal/version.version=${HELM_VERSION} \
        -X helm.sh/helm/v4/internal/version.gitTreeState=clean" \
      helm.sh/helm/v4/cmd/helm@${HELM_VERSION}

# k8s.io/kubernetes uses replace directives, so kubectl can't be built with `go install pkg@version`.
RUN git clone --depth 1 --branch "${KUBECTL_VERSION}" https://github.com/kubernetes/kubernetes.git /src/kubernetes
WORKDIR /src/kubernetes
RUN v="${KUBECTL_VERSION#v}"; major="${v%%.*}"; minor="${v#*.}"; minor="${minor%%.*}"; \
    ldflags="-s -w"; \
    for pkg in k8s.io/client-go/pkg/version k8s.io/component-base/version; do \
      ldflags="${ldflags} -X ${pkg}.gitVersion=${KUBECTL_VERSION} -X ${pkg}.gitMajor=${major} -X ${pkg}.gitMinor=${minor} -X ${pkg}.gitCommit=$(git rev-parse HEAD) -X ${pkg}.gitTreeState=clean"; \
    done; \
    go build -trimpath -ldflags="${ldflags}" -o /out/kubectl ./cmd/kubectl

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
# 20261020 is the first bundle without emSign ECC Root CA - G3 (CVE-2026-111943).
RUN apk add --no-cache 'ca-certificates-bundle>=20261020-r0'
COPY --from=builder /out/helm /out/kubectl /usr/local/bin/
USER 65532:65532
