# One small non-root image holding agent, mcp-books, mcp-evidence and goose
# (CC-1201). Build from the repository root:
#   docker build -f deploy/prod/Dockerfile -t close-copilot/app:prod .
# No .env is copied in: Dockerfile.dockerignore excludes it.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN mkdir /out \
 && mkdir -p /data/results && chown 65532:65532 /data/results \
 && go build -trimpath -ldflags="-s -w" -o /out/agent ./cmd/agent \
 && go build -trimpath -ldflags="-s -w" -o /out/mcp-books ./cmd/mcp-books \
 && go build -trimpath -ldflags="-s -w" -o /out/mcp-evidence ./cmd/mcp-evidence \
 && GOBIN=/out go install -trimpath -ldflags="-s -w" github.com/pressly/goose/v3/cmd/goose@v3.28.0

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY --from=build --chown=65532:65532 /data/results /data/results
COPY config/ /app/config/
COPY migrations/ /app/migrations/
USER nonroot:nonroot
EXPOSE 8000 7001 7002
