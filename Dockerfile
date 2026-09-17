# syntax=docker/dockerfile:1.7
# gator-server image. The runner is not containerised: it drives agent CLIs and git
# worktrees as a real user, so it ships as a binary for systemd (VPS) or launchd (Mac).
ARG GO_VERSION=1.27.1

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w \
        -X github.com/onegator/gator/internal/version.Version=${VERSION} \
        -X github.com/onegator/gator/internal/version.Commit=${COMMIT} \
        -X github.com/onegator/gator/internal/version.Date=${DATE}" \
      -o /out/gator-server ./cmd/gator-server

FROM alpine:3 AS server
# pg_dump/pg_restore/psql power `backup` and `restore`; the client must be at least as new
# as the server, so the major version is a build argument.
ARG PG_CLIENT=postgresql17-client
# git is here for the knowledge registry: packs are fetched with a shallow clone.
RUN apk add --no-cache ca-certificates tzdata git "${PG_CLIENT}" \
 && adduser -D -H -u 10001 gator
COPY --from=build /out/gator-server /usr/local/bin/gator-server
USER gator
ENV GATOR_LISTEN_ADDR=:8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:8080/api/v1/healthz >/dev/null || exit 1
ENTRYPOINT ["gator-server"]
CMD ["serve"]
