FROM golang:1.22-bookworm AS builder
ARG TARGETARCH=amd64

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY bpf ./bpf
COPY configs ./configs

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/netobs-agent ./cmd/netobs-agent

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /
COPY --from=builder /out/netobs-agent /netobs-agent

EXPOSE 9810

ENTRYPOINT ["/netobs-agent"]
CMD ["-listen", ":9810", "-print-events=false"]
