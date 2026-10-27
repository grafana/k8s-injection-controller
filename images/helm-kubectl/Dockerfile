# Built from source because Go's x509 fixes are compiled in; upstream helm/kubectl release binaries lag Go patch releases.
FROM golang:1.26.10@sha256:672c944cfd07599b5f1bb179869270cdc94c6254e41db4d431259693cb286b82 AS builder

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

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
# 20261020 is the first bundle without emSign ECC Root CA - G3 (CVE-2026-111943).
RUN apk add --no-cache 'ca-certificates-bundle>=20261020-r0'
COPY --from=builder /out/helm /out/kubectl /usr/local/bin/
USER 65532:65532
