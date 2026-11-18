FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS go
FROM mcr.microsoft.com/playwright:v1.65.0-noble@sha256:9beb546099e11f52d62e505537914870e01a67e0e6b00ae41da4775324e73dbe
COPY --from=go /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}" \
    GOTOOLCHAIN=local \
    GOENV=off \
    GOWORK=off \
    GOFLAGS=-mod=vendor \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    KONTORA_VISUAL_GO_VERSION=go1.27.4 \
    KONTORA_VISUAL_PLAYWRIGHT_VERSION=1.65.0

# The wrapper stages only this Dockerfile and the browser test npm manifests.
WORKDIR /opt/kontora-visual
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund

WORKDIR /work
RUN mkdir -p test-results/visual-candidates /out/visual /out/playwright-report \
    && chmod 1777 /work test-results
