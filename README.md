<p align="center">
  <img src="./assets/readme/hero.svg" width="100%"
       alt="Timbre — a self-hosted TTS studio. Paste a script, pick a voice, queue the render; the GPU work runs out of band so no request ever waits.">
</p>

**Timbre** is a self-hosted, multi-user web front end for [MossTTS v1.5](https://github.com/sruckh/mossTTS-v1.5-runpod-serverless) running on a RunPod serverless endpoint. You apply for an account, an admin approves it, you paste a script, pick or clone a voice, queue renders, and download WAVs — your own jobs and voices only. The app owns accounts, access control, the queue, the submission, the polling, and the audio.

> **Status: complete.** The whole product is built — it serves, migrates, replicates, injects secrets, gates every route behind account status, ships a voice library (reference upload + the MOSS-TTS v1.5 default voice + cards, global or assigned per user), queues renders, submits out-of-band to RunPod, polls for status (decoding both output shapes the endpoint returns), captures the worker's optional `word_timings` forced alignment, decodes & saves completed WAV audio files, streams downloads scoped to their owner, and handles job deletions — all behind the full studio UI: script editor, parameter fields, voice cards you can rename and audition, a live render queue whose rows load into the player, spoken-line playback that tracks the word being spoken from real per-word timing, and an admin surface for accounts, access requests, and voice ownership. The voice library also ships a whisper.cpp sidecar that transcribes cloned-voice reference audio for Higgs TTS, an engine selector on the compose form, and lazy/eager transcription lifecycle management in the worker. [What works today](#what-works-today) is exact.

## Why it is built this way

Cloudflare cuts off any request at 90 seconds. A cold serverless GPU plus inference takes minutes. Those two facts are irreconcilable if the browser waits on the render — so it doesn't.

<p align="center">
  <img src="./assets/readme/architecture.svg" width="100%"
       alt="Architecture. Browser path: browser to Cloudflare to NGINX Proxy Manager to the Timbre Go app, every hop sub-second. Render path: a background worker POSTs to RunPod's async /run endpoint (MOSS or Higgs) and a poller reads /status/{id} until COMPLETED, then decodes audio_base64, writes a WAV and marks the job ready. For Higgs, the worker first transcribes the voice's reference audio through a private whisper-server sidecar on shared_net. State: SQLite in WAL mode replicated continuously by a Litestream sidecar to an S3-compatible bucket.">
</p>

The browser only ever talks to the Go app, in sub-second hops. A background worker submits to RunPod's async `/run` and a poller watches `/status/{id}`; when it reports `COMPLETED` the worker decodes `audio_base64`, writes the WAV and flips the job to `ready`. The UI finds out by polling the app, not RunPod. `/runsync` is unusable here for the same 90-second reason.

Three consequences worth knowing before you read the code:

- **The container publishes no ports.** It is reachable only on the `shared_net` Docker network, where NGINX Proxy Manager forwards a hostname to it and Cloudflare terminates TLS.
- **Reference audio is sent inline, not via a URL.** The user uploads a sample (drag/drop or file picker); the worker base64-encodes it into the RunPod submission. It is never served from a public URL.
- **A whisper.cpp sidecar transcribes cloned voices.** When the Higgs engine is selected, the worker sends the voice's reference audio to a private `whisper-server` container on `shared_net` over whisper.cpp's HTTP API (`/inference`). The sidecar receives zero secrets and publishes no host ports. MOSS-TTS v1.5 jobs skip transcription entirely and remain operational even when the sidecar is down.

## Accounts and access

Nobody reaches the studio without an approved account, and nobody's jobs or private voices are visible to anyone else. An applicant is a database row an admin has to act on, not a signup that just works.

<p align="center">
  <img src="./assets/readme/access.svg" width="100%"
       alt="Access lifecycle. Applying: a visitor posts the public /apply form, which writes a pending access_requests row and creates no account; POST /register is the API twin and creates a pending account instead — neither route issues a session. Admin: the /admin/ surface approves, denies, or disables, its nav badge counting requests still waiting; the last approved admin cannot be disabled, demoted, or deleted, and no admin can demote themselves. Every gated request: the session passes through the approval gate, which reads role and status live from the database, so an approved user reaches the studio and a pending or disabled one gets the holding screen. A voice card is visible when it is global or assigned to you; jobs and downloads are scoped to the session user.">
</p>

- **Applying creates no session.** `/apply` (browser form) and `POST /register` (its JSON twin) both write an `access_requests` row / a `pending` user and log the applicant straight back out — there is no path from "I filled out a form" to "I'm signed in."
- **The gate reads live, not from the cookie.** `approvalGate` looks up `role` and `status` by primary key on every request behind a session. Demote an admin or disable a user and the very next request sees it — no stale session keeps them in.
- **Admin is a role-gated route group, not a checkbox.** `/admin/` manages user status and role, decides access requests (approve creates the account; deny just records the decision), and controls voice visibility and ownership. Its nav link carries a badge counting requests still waiting for a decision, and is not drawn at all for anyone who isn't an admin — a link guaranteed to answer 403 is not navigation.
- **The last admin is load-bearing.** They cannot be disabled, demoted, or deleted, and no admin can demote their own account — the guard is enforced server-side in the same transaction as the change, not merely hidden in the UI.
- **Voices and jobs are isolated per user.** A voice card is visible when it is `global` or assigned to the requesting user through the `voice_assignments` table; a clone belongs to whoever uploaded it until an admin reassigns or globalizes it. Every job route resolves by session user, so another user's job id is a 404, never a 403 that would confirm it exists.

## What works today

| | |
| --- | --- |
| ✅ HTTP server, `GET /healthz` → `{"ok":true}`, embedded static assets | Serving |
| ✅ SQLite schema — `users` (role, status), `voices` (`is_global`), `voice_assignments`, `access_requests`, `jobs`, `sessions`, with constraints and indexes; additive migrations tested from a fresh install and from the pre-multi-user shape | Migrated on boot |
| ✅ Litestream sidecar replicating to an S3-compatible bucket | Continuous |
| ✅ Secrets injected from Infisical at container start | No secrets in the repo |
| ✅ Multi-stage Docker build — templ + Tailwind + `go build`, no host toolchain | `docker compose` only |
| ✅ Login, session, gated routes — bcrypt admin bootstrap, signed session cookie, everything but `/login`, `/register`, `/apply`, `/healthz`, `/static/*` requires auth | Done |
| ✅ Self-registration & applications — public `/apply` form and `POST /register` both create a `pending` account or request and issue no session; `/apply/status` looks up a decision | Done |
| ✅ Approval gate — every session-bearing request re-reads `role`/`status` from the database; `pending`/`disabled` accounts see a holding screen, never studio data | Done |
| ✅ Admin surface at `/admin/` — role-gated: user status/role, access-request approve/deny/delete, voice global toggle + assignment; last-admin and self-demotion guards enforced server-side | Done |
| ✅ Admin nav badge — the rail's Admin link shows the count of pending access requests, live per page render, hidden for non-admins | Done |
| ✅ Reference file upload, voice library — authenticated multipart upload (type + size validated), stock seed of the MOSS-TTS v1.5 default voice (global by default), cloned voices assigned to their uploader, `/voices` HTML + JSON view, drag/drop + click-to-pick upload | Done |
| ✅ Job queue, scoped per user — authenticated `POST /jobs` inserts a `queued` row owned by the session user and returns the queue fragment; `GET /queue` and `GET /jobs` render only that user's jobs | Done |
| ✅ RunPod submission worker — goroutine started at boot, drains queued jobs under `TIMBRE_MAX_IN_FLIGHT`, POSTs `/run`, stores the returned id, flips to `submitted`/`in_progress`. Submission is idempotent per job and never runs on a browser request | Done |
| ✅ `GET /health` — session-gated probe of the RunPod endpoint's worker pool and queue depth | Done |
| ✅ RunPod status poller & audio capture — polls `/status/{id}`, decodes `audio_base64` on `COMPLETED` to WAV, saves to storage volume (`/data/audio/renders/`), and updates job state to `ready` (or `failed`) | Done |
| ✅ Audio download & job delete routes — owner-scoped `GET /jobs/{id}/audio` streams the WAV; `DELETE /jobs/{id}` removes the DB row and file; another user's job id answers 404 at every one of these | Done |
| ✅ Queue status polling — HTMX `hx-trigger="every 2s"` auto-refreshes queue status in the browser | Done |
| ✅ Studio UI at `/` — script editor with character meter and one-click clear, parameter fields (Seed, Pace, Pitch, Expressiveness, output toggles), a voice-card library and render-queue table that each show ten entries behind a vertical scrollbar, the table fitting the viewport (icon-only row actions, no horizontal scroll), spoken-line playback | Done |
| ✅ Pick a take to play — click, or press Enter/Space on, a queue row to highlight it and load that take into the player. The highlight survives the 2s poll, and the poll never interrupts playback | Done |
| ✅ Word-level playback timing — the poller captures the worker's optional `word_timings` (MMS_FA forced alignment) and stores it on the job; the player highlights the word being spoken from real per-word start/end times, falling back to proportional interpolation for takes that have none | Done |
| ✅ Rename a voice — `POST /voices/{id}/name` replaces the filename a clone was uploaded under. Clones only: renaming a stock voice would be undone by the startup seed | Done |
| ✅ Audition a reference clip — `GET /voices/{id}/reference` plays a cloned voice's source audio from its card. Session-gated and marked private; RunPod still receives the bytes base64-inline, never a URL | Done |
| ✅ Model provenance — `jobs.model` records what rendered each take (backfilled for older rows on migrate) and the player shows it as a badge | Done |
| ✅ Palette-exhaustive test — `internal/web/palette_test.go` fails if any color outside the 10 palette hexes appears in compiled CSS or rendered HTML | Enforced in `go test` |
| ✅ Favicon set — apple-touch-icon, PNG icons, manifest, all embedded and served under `/static/` | Done |
| ✅ Higgs engine support — when `HIGGS_RUNPOD_ENDPOINT` is set, the compose form offers an engine selector (`MOSS-TTS v1.5` default, `bosonai/higgs-tts-3-4b`); the selected model is recorded on the job and routed to the matching RunPod endpoint | Done |
| ✅ whisper.cpp transcription sidecar — private `whisper-server` container compiled from pinned whisper.cpp v1.7.1, model baked in, reached over `/inference` on `shared_net`, zero secrets, no host port | Done |
| ✅ Reference transcription — the worker transcribes cloned-voice reference audio via whisper.cpp (lazy on first Higgs submission, retrying with backoff up to 3 times) and stores the transcript in `voices.reference_transcript`; the voice card shows `Ready` vs `Transcribing...` | Done |
| ✅ MOSS bypass — MOSS-TTS v1.5 jobs never touch the sidecar and remain fully operational during whisper.cpp or Higgs outages | Done |

The complete product works end to end: apply, get approved, paste a script, pick or clone a voice, render on MOSS-TTS v1.5 or Higgs TTS, and download the WAV — with accounts, access control, the queue, submission, polling, alignment capture, transcription, and storage all owned by the app.

## Quick start

Everything builds and runs in Docker. There is no Go, Node, templ or SQLite toolchain on the host, and none is needed.

One command does the lot — loads the Infisical identity, builds, starts the stack, and confirms the keys were injected:

```bash
# First time only: create .env and set at least RUNPOD_ENDPOINT.
cp .env.example .env && $EDITOR .env

scripts/run.sh start    # sources scripts/env.sh → docker compose up -d → verifies
```

`start` is the only correct way to bring the stack up: it builds and starts `whisper-server`, `app`, and `litestream`, waits for both the app's `/healthz` and the whisper-server model load to answer, and then verifies secret injection. `scripts/env.sh` must be sourced **before** `docker compose up` so the machine identity is forwarded into the containers, where `docker/entrypoint.sh` trades it for a token and runs the app under `infisical run`. Skip that and no secrets are injected. Stop with `scripts/run.sh stop`; see `scripts/run.sh` (no args) for `restart`, `status`, `logs`.

Or, step by step (the wrapper runs exactly this):

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

On first boot the app seeds the first admin — `role='admin', status='approved'` — from `ADMIN_USERNAME`/`ADMIN_PASSWORD` (injected by Infisical, or `.env` when running without it) and logs `admin user created` — never the password. Every page except `/login`, `/register`, `/apply`, `/healthz`, `/static/*` then redirects to `/login` until you sign in, and everything but a `status='approved'` account sees a holding screen instead of the studio. Anyone else who wants in applies at `/apply`; approve them from `/admin/`.

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
| `HIGGS_RUNPOD_ENDPOINT` | — | Optional second RunPod endpoint for Higgs TTS v3-4b, e.g. `https://api.runpod.ai/v2/your-higgs-endpoint-id`. When set, the studio shows an engine selector; Higgs jobs route here and reuse `RUNPOD_API_KEY`. Server-side only; never handed to the browser. Leave unset to run MOSS-only. |
| `RUNPOD_API_KEY` | — | Injected by Infisical at container start. Shared by both endpoints. |
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
internal/auth/       bcrypt, admin bootstrap, registration, live role/status lookup, SQLite-backed sessions, middleware, the access_requests store
internal/server/     chi router, login/logout, /register + /apply (public), the approval gate, / studio, /voices + upload + rename + reference preview (owner-scoped), /queue + /jobs + player fragment + audio download & delete (owner-scoped), /admin/ (role-gated: users, access requests, voice ownership, nav badge), static assets
internal/voices/     voice library: MOSS default-voice seed (global, single-model contract), reference upload, per-user visibility via voice_assignments + is_global, admin SetGlobal/Assign/Unassign, rename (clones only), blob storage + read-back, reference transcript get/set/clear
internal/jobs/       jobs table: enqueue + validation (records the rendering model — MOSS or Higgs), claim/submit/fail state transitions, audio & word-timing metadata (`alignment_json`) & deletion, every lookup scoped to a user id
internal/runpod/     the RunPod client — POST /run (MOSS) & POST /run (Higgs), GET /status/{id}, GET /health, output decoded as object-or-aggregate-array (incl. the optional `word_timings` forced-alignment block), permanent-vs-transient errors, payload limits (4 references / 4 MiB each / 6 MiB total, matching RunPod's worker cap)
internal/worker/     background submission worker & status poller loops; the only callers of internal/runpod; whisper.cpp transcription client with claim leases, lazy/eager lifecycle, and MOSS bypass
internal/web/        templ app shell + studio + login + apply + voice library + queue + playback + admin management, Tailwind theme, favicons, palette test (compiled CSS is embedded)
docker/entrypoint.sh Infisical login, then exec the app under `infisical run`
scripts/             env.sh (load identity), run.sh (start/stop the stack with Infisical injection), sync-generated.sh (LSP support)
```

`AGENTS.md` is the operating contract for AI agents working in this repo — build commands, the secret-injection caveat, and the CSS rules live there.

## Notes

- **Editor tooling.** `scripts/sync-generated.sh` copies `go.mod`, `go.sum`, `*_templ.go` and the compiled `app.css` out of the build image so `gopls` can resolve the tree. Generation still only happens in Docker. Re-run it after touching a `.templ` file, `input.css`, or imports.
- **Two health routes, on purpose.** `/healthz` is public, unauthenticated and says nothing about RunPod — it is what the container `HEALTHCHECK` and NGINX Proxy Manager's upstream probe call, and container liveness must not depend on a third party or a RunPod outage would restart a healthy app. `/health` is the operator view: it needs a session, probes the endpoint, and returns `200` with `runpod.reachable: false` rather than a 5xx.
- **The player deliberately sits outside the polled fragment.** The queue refreshes itself by replacing `#queue` wholesale every two seconds, so anything inside it is destroyed on the tick. The player lives in `#playback-body` next to it and is swapped only when you pick a row — inside the queue, audio would restart every two seconds. The same swap is why the queue table has no horizontal scroller: it used to reset your scroll position a few seconds after every scroll, so the table was made to fit instead. The vertical scrollbar got the same treatment when the lists were capped at ten entries: the scroll container wraps `#queue` from the outside as static page markup, because the one time it lived inside the fragment every poll snapped the scrollbar back to the top. Selection survives the tick because the poll sends the selected take back and the server re-renders the highlight.
- **Submitting twice is impossible, not merely unlikely.** `ClaimQueued` only hands out rows with a null `runpod_id`, and recording the id is a compare-and-set, so a duplicate submission can never overwrite the first. `jobs.runpod_id` carries a partial unique index as the last line of defence.
- **Word timing is real, with a graceful fallback.** When the worker returns `word_timings` (forced alignment of the synthesized waveform against the model-normalized transcript), the poller stores it on the job as `alignment_json` and the player walks the playhead word by word from each word's `start`/`end` seconds. The block is optional — older workers, streaming renders, and failed alignments omit it — so the field is a nullable pointer that decodes to "no timing," and the player then interpolates word positions across the audio duration. The spoken line is always rendered from the model's own words, never the caller's input, because the model reflows numbers, punctuation, and pinyin before synthesis.
- **A missing or rejected `RUNPOD_API_KEY` fails jobs; it does not hang them.** That case is classified permanent and fails on the first attempt with the reason on the row. Transient errors (429, 5xx, network) retry three times, counted in `jobs.attempts`, then fail.
- **Streaming.** RunPod exposes `/stream/{id}`; Timbre targets the non-streaming path first — which is also the only path that carries `word_timings` (the worker omits it for streaming renders). Streaming playback is secondary.
- **The whisper-server is compiled, not pulled.** `Dockerfile.whisper` is a multi-stage build that clones pinned whisper.cpp v1.7.1, statically links ggml/whisper (`BUILD_SHARED_LIBS=OFF`), and bakes the `ggml-base.en.bin` model (≈141 MiB) into the image. The `whisper_models` named volume auto-seeds from the image on first `docker compose up` and is mounted read-only at runtime; nothing writes to it.
- **Transcription is lazy and lease-guarded.** A voice's reference audio is transcribed only when a Higgs job needs it and no transcript is stored yet. The worker takes an in-memory claim lease per voice, retries with backoff (5s, 15s, then give up after 3 attempts), and expires a stuck claim after 60s so a crash mid-inference doesn't strand the voice. Transcription can also be triggered eagerly when a job arrives for a voice whose transcript is blank.
- **MOSS is load-bearing and isolated.** MOSS-TTS v1.5 is the default engine and bypasses the transcript gate entirely. The whisper-server going down, `HIGGS_RUNPOD_ENDPOINT` being unset, or Higgs being unavailable never degrades MOSS jobs — the worker skips the transcript check for any job whose model is blank or the MOSS default.
- **Higgs payload limits match RunPod's worker cap.** The RunPod client rejects a Higgs submission before it leaves the process if a reference would exceed RunPod's own server-side limit (4 MiB per `audio_base64` field, 6 MiB total) — failing fast with a clear message rather than sending an oversized clip to be rejected by the worker. `voice` is always `"default"`, never `null`.

## License

This project is licensed under the Apache 2.0 License. MOSS-TTS-v1.5 weights are subject to OpenMOSS Community License terms.

See [`LICENSE`](./LICENSE).
