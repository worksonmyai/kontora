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
FROM ubuntu:25.10

LABEL org.opencontainers.image.title="kontora"
LABEL org.opencontainers.image.description="Agent orchestration daemon for AI coding agents"
LABEL org.opencontainers.image.source="https://github.com/worksonmyai/kontora"

ARG GH_VERSION=2.87.3
ARG DCG_VERSION=0.4.0
ARG OPENCODE_VERSION=1.2.21
ARG NODE_MAJOR=25
ARG PI_VERSION=0.54.2
ARG CLAUDE_CODE_VERSION=2.1.71

WORKDIR /app

# Install runtime dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
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
        amd64) DCG_TRIPLE="x86_64-unknown-linux-gnu" ;; \
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
RUN npm install -g @mariozechner/pi-coding-agent@${PI_VERSION}

# Install Claude Code native binary
RUN ARCH="$(dpkg --print-architecture)" \
    && case "$ARCH" in \
        amd64) CC_PLATFORM="linux-x64" ;; \
        arm64) CC_PLATFORM="linux-arm64" ;; \
        *) echo "Unsupported architecture for Claude Code: $ARCH" >&2; exit 1 ;; \
       esac \
    && CC_GCS="https://storage.googleapis.com/claude-code-dist-86c565f3-f756-42ad-8dfa-d59b1c096819/claude-code-releases" \
    && curl -fsSL "${CC_GCS}/${CLAUDE_CODE_VERSION}/${CC_PLATFORM}/claude" -o /usr/local/bin/claude \
    && chmod +x /usr/local/bin/claude

# Install default kontora config
COPY --chown=ubuntu:ubuntu docker/kontora/config.yaml /home/ubuntu/.config/kontora/config.yaml

# Copy binary from builder
COPY --from=builder /app/kontora /app/kontora

USER ubuntu

ENTRYPOINT ["/app/kontora"]
CMD ["start", "--address", "0.0.0.0"]
