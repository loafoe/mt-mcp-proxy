# Build the proxy binary and run it on a minimal static base.
# The release pipeline uses ko (see .ko.yaml); this Dockerfile exists for local
# `docker compose` and anyone who prefers a plain build.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mt-mcp-proxy ./cmd/mt-mcp-proxy

FROM cgr.dev/chainguard/static:latest
COPY --from=build /out/mt-mcp-proxy /ko-app/mt-mcp-proxy
EXPOSE 8080 9090
ENTRYPOINT ["/ko-app/mt-mcp-proxy"]
CMD ["-config", "/etc/mt-mcp-proxy/config.yaml"]
