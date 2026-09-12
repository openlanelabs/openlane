FROM golang:1.27-alpine AS build
WORKDIR /src
COPY apps/api/ .
RUN CGO_ENABLED=0 go build -o /bin/mcp ./cmd/mcp

FROM alpine:3.24
COPY --from=build /bin/mcp /bin/mcp
# stdio protocol: attach, no ports. OPENLANE_MCP_WORKSPACE + DATABASE_URL
# come from the environment (one process per workspace).
ENTRYPOINT ["/bin/mcp"]
