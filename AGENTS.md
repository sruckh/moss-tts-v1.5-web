# AGENTS.md

Instructions for AI coding agents working in this project.

## Purpose

**Timbre** — a Go web front-end for text-to-speech. A signed-in user pastes a
script, picks or clones a voice, queues render jobs and downloads WAVs.
Inference runs on a RunPod serverless endpoint; this app owns the queue,
submission, polling, audio storage and download/delete.

The plan of record lives in the Outline brain under project
`moss-tts-v1.5-web` — start there. `.goals/` holds the phase-by-phase goal
prompts and the brief/recon they are grounded in, but it is local working
material and is **not** in the published repo, so do not assume it is present.
`DESIGN.md` and `index.html` are the design system — `index.html` is the living
reference, rendered in itself.

## Local Contracts

- **Nothing builds on the host.** Every build, code generation, test and lint
  runs in Docker; `docker compose` is the only entry point. Never run `go
  build`, `go test`, `templ generate`, the Tailwind CLI or `sqlite3` on the host
  to produce an artifact. The one exception is read-only editor tooling — see
  "Editor tooling" below.
- **No published ports.** The app is reachable only on the external
  `shared_net` network, where NGINX Proxy Manager forwards a public hostname to
  it and Cloudflare terminates TLS. Never add a `ports:` mapping.
- **Reference audio is delivered to RunPod as base64 inline, not a public URL.**
  Uploaded samples are stored as blobs and base64-encoded into the RunPod
  submission payload by the worker (confirmed working from testing). There is no
  `/refs/*` route and no `TIMBRE_PUBLIC_BASE_URL`; reference audio never traverses
  Cloudflare → NPM → container. The public hostname (Cloudflare → NPM →
  `timbre-app:8080`, git-ignored `.env`, no default committed) serves only the
  browser UI — nothing about reference audio depends on it.
- **The voice library lives in `internal/voices`.** `internal/voices.Store` owns
  the `voices` table and the reference-audio blobs on the `TIMBRE_AUDIO_DIR`
  volume (`refs/<rand>.<ext>`). On startup it seeds three stock voices
  (Chatterbox/MIT, Qwen3-TTS/Apache-2.0, Higgs Audio v2/Apache-2.0) idempotently.
  The UI is `GET /voices` (HTML page, or JSON when `Accept: application/json`);
  `POST /voices/upload` accepts an authenticated multipart file (validated by
  extension + 10 MB cap), stores the bytes, inserts a `kind='cloned'` row, and
  returns the refreshed grid fragment. Both routes are auth-gated — only
  `/login`, `/healthz`, `/static/*` are exempt. `Store.ReferenceBytes` reads the
  blob back for Goals 4–5's inline base64 submission.
- **No request blocks longer than ~90s** (Cloudflare's cap). The browser talks
  only to this app; the minutes-long RunPod render happens out-of-band in a
  background worker and the UI polls. The browser never calls RunPod.
- **Secrets come from Infisical, never from files in this repo.** See below.
- **The whole UI is gated behind login.** `internal/auth` owns it: bcrypt
  password hashes, a startup bootstrap that seeds the first admin from
  `ADMIN_USERNAME`/`ADMIN_PASSWORD` (Infisical, next to `RUNPOD_API_KEY`) when
  the users table is empty, and SQLite-backed gorilla/sessions with a signed
  session-ID cookie (`HttpOnly`, `SameSite=Lax`, `Secure` when served behind HTTPS (gated by an explicit
  `TIMBRE_SECURE_COOKIES` env; the former `TIMBRE_PUBLIC_BASE_URL` signal is
  removed — see Goal 3); `TIMBRE_SESSION_SECRET` keys the HMAC, also from Infisical). The
  middleware's public exempt list is defined once in `auth.Exempt` — `/login`,
  `/healthz`, `/static/*`. Everything else 302s to `/login` (401 for HTMX/JSON).
- **The palette is exhaustive**: exactly the ten hexes in `DESIGN.md`. New
  values only ever come from `color-mix()` of two of them.

## Work Guidance

### Build, run, test

```sh
. scripts/env.sh                    # loads the Infisical machine identity
docker compose --profile tools build
docker compose up -d
docker compose run --rm test go test ./...
```

`scripts/env.sh` loads the machine-identity credentials from a config directory
**outside this repo** (`~/.config/timbre/` by default; override with
`INFISICAL_CONFIG` / `INFISICAL_SECRET_GPG`). Nothing there is ever committed —
no identity, key, or keyring material belongs in this repository or its history.
Without those credentials the stack still starts, just with no secrets injected.

The `test` service is the build stage plus source, under the `tools` profile so
`docker compose up` never starts it — and so plain `docker compose build` skips
it. Use `--profile tools` when you want it rebuilt.

### Secret injection

The image ships the Infisical CLI. `docker/entrypoint.sh` trades a Universal Auth
machine identity for a short-lived token, then `exec`s the process under
`infisical run`. The identity, project and host come from the environment
(`INFISICAL_CLIENT_ID`, `INFISICAL_CLIENT_SECRET`, `INFISICAL_PROJECT_ID`,
`INFISICAL_DOMAIN`) — never from a file in this repo. Consequences worth
knowing:

- Secrets land in the **app process's** environment, not the container's. A
  fresh `docker compose exec` shell will **not** see `RUNPOD_API_KEY`; read
  `/proc/<pid>/environ` of the running process to verify presence.
- The litestream sidecar runs under the same entrypoint, because its S3
  credentials come from the same place.
- Never bake a token into the image; it would persist in a layer.

### Editor tooling

Serena runs language servers for `go`, `bash`, `yaml` and `scss`
(`.serena/project.yml`). `go` is first, so gopls is the default and fallback.

This is the single sanctioned exception to "nothing on the host": gopls shells
out to `go list`, so the host carries a Go toolchain (`/usr/local/go`, symlinked
into `/usr/local/bin`) purely to answer editor queries. It never produces a
build artifact — `docker compose` still owns every build, generation and test.

gopls also needs the files the build generates, which are not in the tree by
default:

```sh
docker compose --profile tools build && scripts/sync-generated.sh
```

That copies `go.mod`, `go.sum`, `*_templ.go` and `internal/web/app.css` out of
the build image. Re-run it after changing a `.templ` file, `input.css`, or any
import — otherwise gopls reports phantom errors in `internal/web` and anything
that references it. The copies never feed the build: `.dockerignore` excludes
`*_templ.go` and `app.css`, so `docker compose build` always regenerates them.

`.templ` files themselves have no Serena language server; navigate the generated
`*_templ.go` instead.

### CSS

`internal/web/input.css` is the source; the Tailwind v4 CLI compiles it to
`internal/web/app.css` during the build and `internal/web` embeds the result.
The build deliberately does **not** minify — the minifier folds `color-mix()`
into computed hexes, which would put non-palette colors in the output. `@theme
static` forces all ten tokens into the compiled CSS even before anything uses
them.

The only non-palette hexes in `app.css` are Tailwind's own `@property` defaults
for shadow/ring utilities (`#0000`, `#fff`), which this design never uses —
exclude `--tw-*` when checking palette exhaustiveness.

## Verification

Whatever you change, this must still hold:

- `docker compose --profile tools build` and `docker compose up -d` exit 0.
- `docker compose run --rm test go test ./...` exits 0.
- `docker inspect timbre-app --format '{{json .NetworkSettings.Ports}}'` shows
  no `HostPort`, and the container is attached to `shared_net`.
- `/healthz` answers `{"ok":true}`.
- `GET /` without a session cookie answers 302 to `/login`; logging in with
  the bootstrapped admin and replaying the cookie answers 200.
- `docker logs timbre-litestream` shows `replicating to … type=s3` and no errors.

<!-- outline:global-rules (managed by the outline skill) -->
## Global Agent Rules

The shared Global Agent Rules for this brain are imported below. They are
refreshed from Outline into `.outline/global-rules.md` at session start — edit
them in the Outline "Global Agent Rules" page, not here.

@.outline/global-rules.md
<!-- /outline:global-rules -->
