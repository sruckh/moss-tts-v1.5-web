# syntax=docker/dockerfile:1
#
# Nothing here is built on the host: no Go, Node, templ or sqlite3 is installed
# outside these stages. `docker compose build` is the only entry point.

# --- css: Tailwind v4 CLI compiles the palette into internal/web/app.css ------
FROM node:22-alpine AS css
WORKDIR /app
# Installed into /app/node_modules rather than run through npx: the CLI
# resolves `@import "tailwindcss"` from the stylesheet's own directory, so the
# package has to sit on the resolution path above internal/web.
RUN npm install --no-save tailwindcss@4 @tailwindcss/cli@4
# The whole tree, because Tailwind scans .templ/.go for class names.
COPY . .
# Not minified on purpose: the minifier folds color-mix() into computed hexes,
# which would put non-palette colors in app.css. See internal/web/input.css.
RUN ./node_modules/.bin/tailwindcss \
      --input ./internal/web/input.css \
      --output ./internal/web/app.css

# --- build: templ generate, then a static CGO-free binary --------------------
FROM golang:1.25-alpine AS build
WORKDIR /src
# `go install pkg@version` is module-independent, so it runs before the source
# lands and stays cached across source edits.
RUN go install github.com/a-h/templ/cmd/templ@latest
COPY . .
# app.css must exist before `go build`: internal/web embeds it.
COPY --from=css /app/internal/web/app.css ./internal/web/app.css
RUN templ generate
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/timbre ./cmd/timbre

# --- test: the build stage plus source, for `docker compose run --rm test` ---
FROM build AS test
CMD ["go", "test", "./..."]

# --- base: alpine + CA certs + the Infisical CLI + the entrypoint ------------
# Alpine rather than distroless because the entrypoint needs a shell to mint an
# Infisical token before exec'ing the process it wraps. Both the app and the
# litestream sidecar need injected secrets, so both start from here.
FROM alpine:3.21 AS secretbase
RUN apk add --no-cache ca-certificates tzdata wget \
 && wget -qO- 'https://artifacts-cli.infisical.com/setup.apk.sh' | sh \
 && apk add --no-cache infisical
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

# --- litestream: the official binary, started under `infisical run` ----------
# The S3 credentials litestream.yml expands come from Infisical, so the sidecar
# needs the same secret injection the app has.
FROM secretbase AS litestream
COPY --from=litestream/litestream:0.5 /usr/local/bin/litestream /usr/local/bin/litestream
CMD ["/usr/local/bin/litestream", "replicate", "-config", "/etc/litestream.yml"]

# --- runtime: the app --------------------------------------------------------
FROM secretbase AS runtime
RUN adduser -D -u 10001 timbre \
 && mkdir -p /data/audio \
 && chown -R timbre:timbre /data

COPY --from=build /out/timbre /usr/local/bin/timbre

USER timbre
WORKDIR /data
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["/usr/local/bin/timbre"]
