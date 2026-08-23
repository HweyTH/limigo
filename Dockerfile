# syntax=docker/dockerfile:1

FROM golang:1.25 AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/limigo ./cmd/limigo

FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=builder /out/limigo ./limigo
COPY config.example.yaml ./config.example.yaml

EXPOSE 8080 9091
ENTRYPOINT [ "./limigo" ]