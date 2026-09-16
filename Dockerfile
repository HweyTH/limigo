# syntax=docker/dockerfile:1

# The builder always runs on the build host's own architecture and
# cross-compiles for the target: Go needs no emulation to produce an arm64
# binary on amd64 or vice versa, and a multi-platform release build under
# QEMU would otherwise spend minutes emulating the compiler itself.
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/limigo ./cmd/limigo
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/healthcheck ./cmd/healthcheck

FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=builder /out/limigo ./limigo
COPY --from=builder /out/healthcheck ./healthcheck
COPY config.example.yaml ./config.example.yaml

EXPOSE 8080 9091
ENTRYPOINT [ "./limigo" ]