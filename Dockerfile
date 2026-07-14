# Backend server image. Migrations are baked in and applied automatically
# on startup (internal/store.RunMigrations) - no separate migration step
# needed at deploy time.
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/server ./cmd/server
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/pingachock-server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/pingachock-server /usr/local/bin/pingachock-server
COPY migrations /migrations
ENV MIGRATIONS_DIR=/migrations
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/pingachock-server"]
