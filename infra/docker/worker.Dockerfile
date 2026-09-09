FROM golang:1.27-alpine AS build
WORKDIR /src
COPY apps/worker/ .
RUN CGO_ENABLED=0 go build -o /bin/worker ./cmd/worker

FROM alpine:3.24
COPY --from=build /bin/worker /bin/worker
ENTRYPOINT ["/bin/worker"]
