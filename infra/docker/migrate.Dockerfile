FROM golang:1.27-alpine
WORKDIR /src
# goose + river pinned per spec §19 toolchain (ADR-0003: river's own
# migrator owns its schema — goose can't express v7's enum add+use).
# postgresql16-client for the one-time app-role grants on river tables.
RUN apk add --no-cache postgresql16-client && \
    go install github.com/pressly/goose/v3/cmd/goose@v3.22.0 && \
    go install github.com/riverqueue/river/cmd/river@v0.47.0
COPY db/migrations/ /src/migrations/
ENTRYPOINT ["sh", "-c", "goose -dir /src/migrations postgres \"$DATABASE_URL\" up && \
river migrate-up --database-url \"$DATABASE_URL\" && \
psql \"$DATABASE_URL\" -c \"GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE river_job, river_queue, river_leader, river_migration, river_notification TO openlane_app; GRANT USAGE, SELECT ON SEQUENCE river_job_id_seq, river_notification_id_seq TO openlane_app;\""]
