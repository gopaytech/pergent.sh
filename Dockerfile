FROM golang:1.24-bookworm AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /pergent-sh ./cmd/pergent-sh

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /pergent-sh /pergent-sh

EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/pergent-sh", "serve"]
