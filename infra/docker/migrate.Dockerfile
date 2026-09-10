FROM golang:1.27-alpine
WORKDIR /src
# goose pinned per spec §19 toolchain
RUN go install github.com/pressly/goose/v3/cmd/goose@v3.22.0
COPY db/migrations/ /src/migrations/
ENTRYPOINT ["sh", "-c", "goose -dir /src/migrations postgres \"$DATABASE_URL\" up"]
