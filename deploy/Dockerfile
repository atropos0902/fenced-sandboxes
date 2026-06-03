FROM golang:1.26.2-alpine3.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/ ./cmd/...

FROM alpine:3.23.3 AS fenced
RUN apk add --no-cache ca-certificates && adduser -D -u 10002 fenced
COPY --from=build /out/fenced /usr/local/bin/fenced
USER fenced
ENTRYPOINT ["fenced"]

FROM alpine:3.23.3 AS fakegithub
RUN apk add --no-cache git git-daemon
COPY --from=build /out/fakegithub /usr/local/bin/fakegithub
COPY deploy/seed /seed
ENTRYPOINT ["fakegithub"]

FROM node:24.13.0-alpine3.23 AS sandbox
RUN apk add --no-cache git curl && adduser -D -u 10001 sandbox && mkdir /work && chown sandbox:sandbox /work
USER sandbox
ENV npm_config_cache=/home/sandbox/.npm npm_config_update_notifier=false npm_config_fund=false npm_config_audit=false
WORKDIR /work
