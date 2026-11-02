# Bases pinned by digest, the same as the yarilo image; dependabot moves them.
FROM golang:1.26.10-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS builder

# A newer go.mod must fail the build, not fetch a toolchain.
ENV GOTOOLCHAIN=local
WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY main.go metrics.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath -o mailfrom-milter .

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

# hadolint ignore=DL3018,DL3019
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /build/mailfrom-milter .

USER 65534:65534
EXPOSE 10031/tcp

CMD ["/app/mailfrom-milter"]
