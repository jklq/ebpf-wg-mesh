FROM golang:1.26-bookworm AS build

ENV PATH=/usr/local/go/bin:${PATH}
WORKDIR /src

RUN apt-get update \
    && apt-get install -y --no-install-recommends clang llvm libelf-dev libbpf-dev linux-libc-dev \
    && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go generate ./internal/firewall
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/meshd ./cmd/meshd
RUN CGO_ENABLED=0 GOOS=linux go install github.com/containerd/nerdctl/cmd/nerdctl@v1.7.7

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
    ca-certificates \
    containerd \
    containernetworking-plugins \
    curl \
    iproute2 \
    iptables \
    iputils-ping \
    netcat-openbsd \
    procps \
    python3 \
    runc \
    tcpdump \
    wireguard-tools \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/meshd /usr/local/bin/meshd
COPY --from=build /go/bin/nerdctl /usr/local/bin/nerdctl
WORKDIR /app
ENTRYPOINT ["/usr/local/bin/meshd"]
