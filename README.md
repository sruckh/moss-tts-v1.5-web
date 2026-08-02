<p align="center">
  <img src="./assets/readme/hero.svg" width="100%"
       alt="Timbre — a self-hosted TTS studio. Paste a script, pick a voice, queue the render; the GPU work runs out of band so no request ever waits.">
</p>

**Timbre** is a self-hosted web front end for [MossTTS v1.5](https://github.com/sruckh/mossTTS-v1.5-runpod-serverless) running on a RunPod serverless endpoint. You paste a script, pick or clone a voice, queue renders, and download WAVs. The app owns the queue, the submission, the polling, and the audio.

> **Status: complete.** The whole product is built and deployed at [timbre.gemneye.xyz](https://timbre.gemneye.xyz) — it serves, migrates, replicates, injects secrets, authenticates, ships a voice library (reference upload + the MOSS-TTS v1.5 default voice + cards), queues renders, submits out-of-band to RunPod, polls for status (decoding both output shapes the endpoint returns), decodes & saves completed WAV audio files, streams downloads, and handles job deletions — all behind the full studio UI: script editor, parameter fields, voice cards, a live render queue, and spoken-line playback. [What works today](#what-works-today) is exact.

## Why it is built this way

Cloudflare cuts off any request at 90 seconds. A cold serverless GPU plus inference takes minutes. Those two facts are irreconcilable if the browser waits on the render — so it doesn't.

<p align="center">
  <img src="./assets/readme/architecture.svg" width="100%"
       alt="Architecture. Browser path: browser to Cloudflare to NGINX Proxy Manager to the Timbre Go app, every hop sub-second. Render path: a background worker POSTs to RunPod's async /run endpoint and a poller reads /status/{id} until COMPLETED, then decodes audio_base64, writes a WAV and marks the job ready. State: SQLite in WAL mode replicated continuously by a Litestream sidecar to an S3-compatible bucket.">
</p>

The browser only ever talks to the Go app, in sub-second hops. A background worker submits to RunPod's async `/run` and a poller watches `/status/{id}`; when it reports `COMPLETED` the worker decodes `audio_base64`, writes the WAV and flips the job to `ready`. The UI finds out by polling the app, not RunPod. `/runsync` is unusable here for the same 90-second reason.

Two consequences worth knowing before you read the code:

- **The container publishes no ports.** It is reachable only on the `shared_net` Docker network, where NGINX Proxy Manager forwards a hostname to it and Cloudflare terminates TLS.
- **Reference audio is sent inline, not via a URL.** The user uploads a sample (drag/drop or file picker); the worker base64-encodes it into the RunPod submission. It is never served from a public URL.

## What works today

| | |
| --- | --- |
| ✅ HTTP server, `GET /healthz` → `{"ok":true}`, embedded static assets | Serving |
| ✅ SQLite schema — `users`, `voices`, `jobs`, `sessions` with constraints and indexes | Migrated on boot |
| ✅ Litestream sidecar replicating to an S3-compatible bucket | Continuous |
| ✅ Secrets injected from Infisical at container start | No secrets in the repo |
| ✅ Multi-stage Docker build — templ + Tailwind + `go build`, no host toolchain | `docker compose` only |
| ✅ Login, session, gated routes — bcrypt admin bootstrap, signed session cookie, everything but `/login`, `/healthz`, `/static/*` requires auth | Done |
| ✅ Reference file upload, voice library — authenticated multipart upload (type + size validated), stock seed of the MOSS-TTS v1.5 default voice, cloned voices from uploaded references, `/voices` HTML + JSON view, drag/drop + click-to-pick upload | Done |
| ✅ Job queue — authenticated `POST /jobs` inserts a `queued` row and returns the queue fragment; `GET /queue` and `GET /jobs` render it | Done |
| ✅ RunPod submission worker — goroutine started at boot, drains queued jobs under `TIMBRE_MAX_IN_FLIGHT`, POSTs `/run`, stores the returned id, flips to `submitted`/`in_progress`. Submission is idempotent per job and never runs on a browser request | Done |
| ✅ `GET /health` — session-gated probe of the RunPod endpoint's worker pool and queue depth | Done |
| ✅ RunPod status poller & audio capture — polls `/status/{id}`, decodes `audio_base64` on `COMPLETED` to WAV, saves to storage volume (`/data/audio/renders/`), and updates job state to `ready` (or `failed`) | Done |
| ✅ Audio download & job delete routes — authenticated `GET /jobs/{id}/audio` streams WAV file; `DELETE /jobs/{id}` removes DB row and audio file from volume | Done |
| ✅ Queue status polling — HTMX `hx-trigger="every 2s"` auto-refreshes queue status in the browser | Done |
| ✅ Studio UI at `/` — script editor with character meter, parameter fields (Seed, Pace, Pitch, Expressiveness, output toggles), voice-card library, live render-queue table, spoken-line playback with Download WAV | Done |
| ✅ Palette-exhaustive test — `internal/web/palette_test.go` fails if any color outside the 10 palette hexes appears in compiled CSS or rendered HTML | Enforced in `go test` |
| ✅ Favicon set — apple-touch-icon, PNG icons, manifest, all embedded and served under `/static/` | Done |

The complete product is live: paste a script, pick or clone a voice, render on MOSS-TTS v1.5, and download the WAV — with the queue, submission, polling, and storage owned by the app.

## Quick start

Everything builds and runs in Docker. There is no Go, Node, templ or SQLite toolchain on the host, and none is needed.

```bash
# 1. Set your deployment values. .env is git-ignored.
cp .env.example .env && $EDITOR .env

# 2. Put the machine identity for your secrets backend in the shell.
. scripts/env.sh

# 3. Build (--profile tools also builds the test image) and start.
docker compose --profile tools build
docker compose up -d

# 4. Confirm it is answering.
docker compose exec app wget -qO- http://localhost:8080/healthz
# => {"ok":true}
```

On first boot the app seeds the admin user from `ADMIN_USERNAME`/`ADMIN_PASSWORD` (injected by Infisical, or `.env` when running without it) and logs `admin user created` — never the password. Every page except `/login`, `/healthz`, `/static/*` then redirects to `/login` until you sign in.

Run the tests:

```bash
docker compose run --rm test go test ./...
```

Use the **`test`** service, not `app` — the `app` image is a slim alpine runtime with no Go toolchain in it.

You will need an external Docker network named `shared_net` (`docker network create shared_net` if you do not already have one) and an NGINX Proxy Manager entry pointing your hostname at `timbre-app:8080`.

## Configuration

Read from the environment at startup:

| Variable | Default | Purpose |
| --- | --- | --- |
| `TIMBRE_ADDR` | `:8080` | Listen address. Not published to the host. |
| `TIMBRE_DB_PATH` | `/data/timbre.db` | SQLite file, on the volume Litestream shares. |
| `TIMBRE_AUDIO_DIR` | `/data/audio` | Rendered WAVs and uploaded reference samples. |
| `TIMBRE_SECURE_COOKIES` | — | Set `true` when served behind HTTPS so session cookies carry the `Secure` flag. It is an explicit opt-in rather than inferred from a public hostname, because reference audio goes to RunPod base64-inline and the app needs no public URL of its own. |
| `RUNPOD_ENDPOINT` | — | Your endpoint, e.g. `https://api.runpod.ai/v2/your-endpoint-id`. Server-side only; never handed to the browser. |
| `RUNPOD_API_KEY` | — | Injected by Infisical at container start. |
| `ADMIN_USERNAME` | — | Seeds the first admin user on startup (only when the users table is empty). Injected by Infisical. |
| `ADMIN_PASSWORD` | — | Password for that bootstrap admin, bcrypt-hashed at rest. Injected by Infisical. |
| `TIMBRE_SESSION_SECRET` | — | Keys the HMAC that signs session cookies. Injected by Infisical; if unset, sessions are forgotten on restart. |
| `TIMBRE_MAX_IN_FLIGHT` | `2` | How many jobs the worker keeps submitted at once. |

`RUNPOD_API_KEY`, the three auth secrets above and the replication credentials come from [Infisical](https://infisical.com) via the container entrypoint, which trades a Universal Auth machine identity for a short-lived token and runs the app under `infisical run`. Nothing is baked into an image layer.

One consequence that costs people an afternoon: `infisical run` injects into the **app process**, not the container. A fresh `docker compose exec` shell will not see `RUNPOD_API_KEY` even when injection worked. Check the process instead:

```bash
docker compose exec -T app sh -c \
  'for p in /proc/[0-9]*; do tr "\0" "\n" < $p/environ 2>/dev/null \
     | grep -q "^RUNPOD_API_KEY=." && echo present && break; done'
```

## Design system

Timbre has a written design system with an unusually strict rule: **ten colors, and nothing else.** Every other value is a `color-mix()` of two of them — no neutral gray, no sampled hex.

| | | | | |
| --- | --- | --- | --- | --- |
| `#00003E` Ink | `#D5D5F4` Paper | `#4A4AC8` Indigo | `#9EB600` Olive | `#006795` Teal |
| `#C98500` Amber | `#009044` Green | `#C95D00` Burnt | `#83008C` Purple | `#0000FF` Blue |

Olive is the brightest value against Ink, so it marks the word being spoken *right now* and doubles as the focus ring. Amber → Green is the render lifecycle. Burnt is refusal, not danger. Blue appears exactly once, in the masthead spectrum, where nothing depends on reading it.

- [`DESIGN.md`](./DESIGN.md) — the full system: palette rationale, measured contrast, type scale, components.
- [`index.html`](./index.html) — the living reference; the system rendered in itself.

The build compiles `internal/web/input.css` to `app.css` with the Tailwind v4 CLI and **does not minify it**, deliberately: the minifier folds `color-mix()` into computed hexes, which would put colors in the output that belong to no palette. The rule is enforced mechanically — `internal/web/palette_test.go` scans the compiled CSS and the rendered HTML of every page and fails the suite if a foreign hex appears (Tailwind's own `--tw-*`/`@property` boilerplate excluded).

## Layout

```
cmd/timbre/          entry point, graceful shutdown
internal/config/     environment config; reports key presence, never the value
internal/db/         driver, pragmas, schema, migrations
internal/auth/       bcrypt, admin bootstrap, SQLite-backed sessions, middleware
internal/server/     chi router, login/logout, /healthz + /health, / studio, /voices + upload, /queue + /jobs + audio download & delete, static assets
internal/voices/     voice library: MOSS default-voice seed (single-model contract), reference upload, blob storage + read-back
internal/jobs/       jobs table: enqueue + validation, claim/submit/fail state transitions, audio metadata & deletion
internal/runpod/     the RunPod client — POST /run, GET /status/{id}, GET /health, output decoded as object-or-aggregate-array, permanent-vs-transient errors
internal/worker/     background submission worker & status poller loops; the only callers of internal/runpod
internal/web/        templ app shell + studio + login + voice library + queue + playback, Tailwind theme, favicons, palette test (compiled CSS is embedded)
docker/entrypoint.sh Infisical login, then exec the app under `infisical run`
scripts/             env.sh (load identity), sync-generated.sh (LSP support)
```

`AGENTS.md` is the operating contract for AI agents working in this repo — build commands, the secret-injection caveat, and the CSS rules live there.

## Notes

- **Editor tooling.** `scripts/sync-generated.sh` copies `go.mod`, `go.sum`, `*_templ.go` and the compiled `app.css` out of the build image so `gopls` can resolve the tree. Generation still only happens in Docker. Re-run it after touching a `.templ` file, `input.css`, or imports.
- **Two health routes, on purpose.** `/healthz` is public, unauthenticated and says nothing about RunPod — it is what the container `HEALTHCHECK` and NGINX Proxy Manager's upstream probe call, and container liveness must not depend on a third party or a RunPod outage would restart a healthy app. `/health` is the operator view: it needs a session, probes the endpoint, and returns `200` with `runpod.reachable: false` rather than a 5xx.
- **Submitting twice is impossible, not merely unlikely.** `ClaimQueued` only hands out rows with a null `runpod_id`, and recording the id is a compare-and-set, so a duplicate submission can never overwrite the first. `jobs.runpod_id` carries a partial unique index as the last line of defence.
- **A missing or rejected `RUNPOD_API_KEY` fails jobs; it does not hang them.** That case is classified permanent and fails on the first attempt with the reason on the row. Transient errors (429, 5xx, network) retry three times, counted in `jobs.attempts`, then fail.
- **Streaming.** RunPod exposes `/stream/{id}`; Timbre targets the non-streaming path first. Streaming playback is secondary.

## License

This project is licensed under the Apache 2.0 License. MOSS-TTS-v1.5 weights are subject to OpenMOSS Community License terms.

See [`LICENSE`](./LICENSE).
