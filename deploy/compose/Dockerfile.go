# One builder, two binaries: the gateway and the demo tool service. They share a module, so
# building them twice would only double the cache misses.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY services/gateway-go/go.mod ./
COPY services/gateway-go/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway . \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mcp-demo ./cmd/mcp-demo

FROM alpine:3.20
RUN adduser -D -u 10001 app && mkdir -p /state /config && chown -R app /state
COPY --from=build /out/gateway /usr/local/bin/gateway
COPY --from=build /out/mcp-demo /usr/local/bin/mcp-demo
USER app
WORKDIR /state
