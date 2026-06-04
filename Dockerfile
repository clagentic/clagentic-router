# Dockerfile — clagentic-router
#
# API-only mode: HTTP-based adapters only (anthropic_api, openai_api, ollama_http).
# CLI adapters (claude_cli, codex_cli, codex_subagent) require OAuth sessions and
# can't run in a container — deploy those on the host directly (see README.md).
#
# Build:
#   docker build -t clagentic-router .
#
# Run:
#   docker run -p 8765:8765 \
#     -v /path/to/router.yaml:/etc/clagentic-router/router.yaml:ro \
#     -e CLAGENTIC_ROUTER_TOKEN=secret \
#     -e ANTHROPIC_API_KEY=sk-... \
#     clagentic-router

FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /clagentic-router ./cmd/clagentic-router

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /clagentic-router /usr/local/bin/clagentic-router

EXPOSE 8765
VOLUME ["/var/lib/clagentic-router"]

ENV CLAGENTIC_ROUTER_CONFIG=/etc/clagentic-router/router.yaml

ENTRYPOINT ["clagentic-router", "serve"]
