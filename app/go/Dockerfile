# Bases pinned by digest, the same as the yarilo image; dependabot moves them.
FROM golang:1.26.10-alpine@sha256:27f2d3b83cfc5803c8078f8e730af0f7c431bf067afaec8f60a36fb763187cba AS builder

# A newer go.mod must fail the build, not fetch a toolchain.
ENV GOTOOLCHAIN=local
WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY main.go metrics.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath -o mailfrom-milter .

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# hadolint ignore=DL3018,DL3019
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /build/mailfrom-milter .

USER 65534:65534
EXPOSE 10031/tcp

CMD ["/app/mailfrom-milter"]
