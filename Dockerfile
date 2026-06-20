# Build stage
FROM golang:1.26-bookworm AS builder

ARG VERSION=dev

WORKDIR /app

# Cache Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build with optimized flags
RUN go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o kontora ./cmd/kontora

# Runtime stage
FROM ubuntu:26.04

LABEL org.opencontainers.image.title="kontora"
LABEL org.opencontainers.image.description="Agent orchestration daemon for AI coding agents"
LABEL org.opencontainers.image.source="https://github.com/worksonmyai/kontora"

ARG GH_VERSION=2.95.0
ARG DCG_VERSION=0.5.7
ARG OPENCODE_VERSION=1.17.8
ARG NODE_MAJOR=26
ARG PI_VERSION=0.79.8
ARG CLAUDE_CODE_VERSION=2.1.183
ARG CLAUDE_CODE_SHA256_AMD64=df3b409c5b25299df52c5ee81f64811dbdcb2e18c1beefe7f733c326f0a8cdce
ARG CLAUDE_CODE_SHA256_ARM64=260a6e43fe9c6fd8800317581982ff50e4f4401d02ef625faa4df723bb9710b3

WORKDIR /app

# Install runtime dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    openssh-client \
    tmux \
    xz-utils \
    && rm -rf /var/lib/apt/lists/*

# Install GitHub CLI (gh)
RUN curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_$(dpkg --print-architecture).deb" -o /tmp/gh.deb \
    && dpkg -i /tmp/gh.deb \
    && rm /tmp/gh.deb

# Install destructive command guard (dcg)
RUN ARCH="$(dpkg --print-architecture)" \
    && case "$ARCH" in \
        amd64) DCG_TRIPLE="x86_64-unknown-linux-musl" ;; \
        arm64) DCG_TRIPLE="aarch64-unknown-linux-gnu" ;; \
        *) echo "Unsupported architecture for dcg: $ARCH" >&2; exit 1 ;; \
       esac \
    && curl -fsSL "https://github.com/Dicklesworthstone/destructive_command_guard/releases/download/v${DCG_VERSION}/dcg-${DCG_TRIPLE}.tar.xz" \
       -o /tmp/dcg.tar.xz \
    && tar -xJf /tmp/dcg.tar.xz -C /usr/local/bin/ dcg \
    && chmod +x /usr/local/bin/dcg \
    && rm /tmp/dcg.tar.xz

# Install opencode
RUN ARCH="$(dpkg --print-architecture)" \
    && case "$ARCH" in \
        amd64) OC_ARCH="x64" ;; \
        arm64) OC_ARCH="arm64" ;; \
        *) echo "Unsupported architecture for opencode: $ARCH" >&2; exit 1 ;; \
       esac \
    && curl -fsSL "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/opencode-linux-${OC_ARCH}.tar.gz" \
       -o /tmp/opencode.tar.gz \
    && tar -xzf /tmp/opencode.tar.gz -C /usr/local/bin/ opencode \
    && chmod +x /usr/local/bin/opencode \
    && rm /tmp/opencode.tar.gz

# Install Node.js (required for pi)
RUN curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Install pi coding agent
RUN npm install -g @earendil-works/pi-coding-agent@${PI_VERSION}

# Install Claude Code native binary
RUN ARCH="$(dpkg --print-architecture)" \
    && case "$ARCH" in \
        amd64) CC_PLATFORM="linux-x64"; CC_SHA256="$CLAUDE_CODE_SHA256_AMD64" ;; \
        arm64) CC_PLATFORM="linux-arm64"; CC_SHA256="$CLAUDE_CODE_SHA256_ARM64" ;; \
        *) echo "Unsupported architecture for Claude Code: $ARCH" >&2; exit 1 ;; \
       esac \
    && CC_GCS="https://storage.googleapis.com/claude-code-dist-86c565f3-f756-42ad-8dfa-d59b1c096819/claude-code-releases" \
    && curl -fsSL "${CC_GCS}/${CLAUDE_CODE_VERSION}/${CC_PLATFORM}/claude" -o /usr/local/bin/claude \
    && echo "${CC_SHA256}  /usr/local/bin/claude" | sha256sum -c - \
    && chmod +x /usr/local/bin/claude

# Install default kontora config
COPY --chown=ubuntu:ubuntu docker/kontora/config.yaml /home/ubuntu/.config/kontora/config.yaml

# Copy binary from builder
COPY --from=builder /app/kontora /app/kontora

USER ubuntu

ENTRYPOINT ["/app/kontora"]
CMD ["start", "--address", "0.0.0.0"]
