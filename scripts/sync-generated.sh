#!/bin/sh
# Copy build-generated files out of the build image into the working tree.
#
# Generation still happens only in Docker — this just makes the results visible
# to editors and to gopls, which cannot resolve internal/web (templ output) or
# the module graph (go.sum) without them. Nothing here feeds the build: .dockerignore
# excludes *_templ.go and app.css, so `docker compose build` regenerates both.
#
# Run after any change to a .templ file, to input.css, or to the imports:
#
#     docker compose --profile tools build && scripts/sync-generated.sh
set -eu

IMAGE="${IMAGE:-timbre-test:latest}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
	echo "sync-generated: image $IMAGE not found — run 'docker compose --profile tools build' first" >&2
	exit 1
fi

CID="$(docker create "$IMAGE")"
trap 'docker rm -f "$CID" >/dev/null 2>&1 || true' EXIT

# Resolved module graph: `go mod tidy` runs in the build stage, so these are the
# authoritative versions.
docker cp "$CID:/src/go.mod" "$ROOT/go.mod"
docker cp "$CID:/src/go.sum" "$ROOT/go.sum"

# templ output and the compiled stylesheet that internal/web embeds.
for f in $(docker export "$CID" | tar -t 2>/dev/null | grep -E '^src/internal/.*_templ\.go$' || true); do
	docker cp "$CID:/$f" "$ROOT/${f#src/}"
done
docker cp "$CID:/src/internal/web/app.css" "$ROOT/internal/web/app.css"

echo "sync-generated: refreshed go.mod, go.sum, *_templ.go and internal/web/app.css from $IMAGE"
