# Multi-arch: linux/amd64 and linux/arm64. The web and Go stages run on the
# build machine (the dashboard build is the same for every platform; Go
# cross-compiles), so no stage needs emulation.
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS web

# vite.config.ts writes the build to ../internal/web/dist.
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.24.8-bookworm AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/web/dist ./internal/web/dist
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -o /pergent-sh ./cmd/pergent-sh

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /pergent-sh /pergent-sh

EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/pergent-sh", "serve"]
