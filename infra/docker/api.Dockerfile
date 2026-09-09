FROM golang:1.22-alpine AS build
WORKDIR /src
COPY apps/api/ .
RUN CGO_ENABLED=0 go build -o /bin/api ./cmd/api

FROM alpine:3.24
COPY --from=build /bin/api /bin/api
EXPOSE 8080
ENTRYPOINT ["/bin/api"]
