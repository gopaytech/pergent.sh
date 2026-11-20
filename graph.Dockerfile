# The code graph build image (docs/concepts/code-graph.md): graphify, pinned,
# and the pergent CLI at the version go.mod pins, so `pergent graph convert`
# writes the pergent-graph/v1 that pergent.sh reads. It runs in the graphify
# container of a graph build Job, which holds no token and no LLM key.
# Multi-arch: linux/amd64 and linux/arm64. The Go stage runs on the build
# machine and cross-compiles; the Python stage runs on the target platform.
FROM --platform=$BUILDPLATFORM golang:1.24.8-bookworm AS pergent

ARG TARGETOS
ARG TARGETARCH

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
# The shell strips the version's "v", as pergent's own Dockerfile does.
RUN v="$(go list -m -f '{{.Version}}' github.com/gopaytech/pergent)" && v="${v#v}" \
    && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
       go build -trimpath -ldflags "-X main.version=$v" -o /pergent github.com/gopaytech/pergent/cmd

FROM python:3.12-slim-bookworm

# graphify reads the output of exactly one version: pergent's converter
# refuses any other. Change it only with the pergent release that reads it.
ARG GRAPHIFY_VERSION=0.9.112
ENV PYTHONDONTWRITEBYTECODE=1 PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
RUN pip install "graphifyy==${GRAPHIFY_VERSION}" && graphify --help > /dev/null

COPY --from=pergent /pergent /usr/local/bin/pergent

USER 65532:65532
WORKDIR /out
