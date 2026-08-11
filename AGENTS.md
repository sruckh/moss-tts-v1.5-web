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
`.icm/higgs-tts-whisper/` is the local, planning-only ICM pipeline for Higgs
reference transcription and post-render word alignment; stage outputs are
human-reviewed handoffs, and stage edits require ICM `sync` then `audit`.
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
- **Whisper is a private, secret-free sidecar.** `whisper-server` builds from
  `Dockerfile.whisper`, joins only `shared_net`, publishes no host port and
  receives no RunPod, Infisical, auth/session or S3 credentials. The pinned
  whisper.cpp v1.7.1 server has no `/health` route; its Docker healthcheck probes
  `GET /`, while transcription uses `POST /inference`. Stopping only the sidecar
  is the rollback fallback: MOSS jobs and the app UI stay available; restore it
  with `docker compose up -d --no-deps whisper-server` and wait for `healthy`.
- **Reference audio is delivered to RunPod as base64 inline, not a public URL.**
  Uploaded samples are stored as blobs and base64-encoded into the RunPod
  submission payload by the worker (confirmed working from testing). There is no
  `/refs/*` route and no `TIMBRE_PUBLIC_BASE_URL`; reference audio never traverses
  Cloudflare → NPM → container. The public hostname (Cloudflare → NPM →
  `timbre-app:8080`, git-ignored `.env`, no default committed) serves only the
  browser UI — nothing about reference audio depends on it.
  The one route that returns reference bytes is `GET /voices/{id}/reference`:
  session-gated, `Content-Disposition: inline`, `Cache-Control: private`, 404 for
  stock voices. It exists so a card can play what a voice was cloned from. It is
  not a public URL and RunPod is never given a link — submission still carries
  the bytes base64-inline.
- **The voice library lives in `internal/voices`.** `internal/voices.Store` owns
  the `voices` table and the reference-audio blobs on the `TIMBRE_AUDIO_DIR`
  volume (`refs/<rand>.<ext>`). On startup it seeds one stock voice —
  **Moss, the MOSS-TTS v1.5 default voice** (no reference audio; the MOSS
  endpoint renders its built-in voice) — and reconciles away any stale stock
  rows. MOSS-TTS v1.5 remains the default engine; the studio also exposes
  `bosonai/higgs-tts-3-4b`, whose voice identity comes from cloned references.
  The UI is `GET /voices` (HTML page, or JSON when `Accept: application/json`);
  `POST /voices/upload` accepts an authenticated multipart file (validated by
  extension + 10 MB cap), stores the bytes, inserts a `kind='cloned'` row,
  assigns the new card to the session user, and returns the refreshed grid
  fragment; `POST /voices/{id}/name` renames a voice
  and returns the same fragment. **Rename is clones-only** — `SeedStock`
  reconciles stock rows *by name*, so a renamed stock row would read as stale on
  the next boot and be deleted, taking every job's voice link with it
  (`ON DELETE SET NULL`); `Store.Rename` returns `ErrNotRenamable` instead. All
  routes are auth-gated — only `/login`, `/healthz`, `/static/*` are exempt.
  `Store.ReferenceBytes` reads the blob back for inline base64 submission.
  Cloned cards report transcription readiness from `ReferenceTranscript`:
  non-blank stored text is **Ready**, otherwise **Transcribing...**. Upload and
  enqueue responses never wait for Whisper; the background worker owns eager and
  atomic lazy recovery before a Higgs job reaches RunPod. MOSS jobs bypass this
  transcript gate entirely.
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
  `/register` is exempt too — applying for an account cannot require one.
  **The session carries `user_id`, `role` and `status`,** snapshotted at login.
  Read them via `auth.Manager.Current(r)` (one session load) rather than `Role`
  then `Status` (two); `UserID(r)` is unchanged. Use the
  `auth.Role*`/`auth.Status*` constants, never a bare string. Because role and
  status snapshots can go stale, authorization gates use
  `auth.Manager.LiveIdentity` on every protected request: approving, disabling,
  promoting or demoting an account takes effect on its next request without a
  session purge. `Login` deliberately admits `pending` and `disabled` users:
  signing in and reaching the studio are separate questions, and the holding
  screen needs a session to know who is waiting.
  **`Bootstrap` writes `role='admin', status='approved'` explicitly.** It runs
  *after* `db.Migrate` on a fresh install, so the migration's re-promotion of the
  lowest-id user has already passed over an empty table; leaving this row to the
  column defaults would strand the first admin as `pending` until a restart.
  **`POST /register` grants nothing.** It creates a `role='user', status='pending'`
  row (written explicitly, not via defaults) and issues **no session cookie** —
  anything that signs the applicant in is a bug. It answers JSON (201/400/409),
  since the public form is a later concern. Duplicate usernames are caught by the
  `users.username` UNIQUE constraint, not a prior SELECT, which two concurrent
  applicants could both pass.
- **The schema is multi-user-aware, and migrations stay additive.** `users`
  carries `role` (`admin|user`) and `status` (`approved|pending|disabled`,
  defaulting to `pending`) plus an optional `email`; `voices` keeps the nullable
  `owner_id` column for schema compatibility and carries `is_global`, but access
  no longer comes from `owner_id`. `voice_assignments(voice_id, user_id)` is the
  many-to-many access source, unique per pair with cascading foreign keys;
  visibility is `is_global = 1 OR voice_assignments.user_id = ?` and list queries
  use `DISTINCT` so a global assigned card appears once. `access_requests` holds
  an application from someone who has no account yet. Three migration data fixes
  have distinct lifecycles: the lowest-id user is restored to
  `role='admin', status='approved'` on **every** pass; stock voices are made
  global **exactly once**, when `is_global` first appears; and legacy non-NULL
  `owner_id` values are copied with `INSERT OR IGNORE` into
  `voice_assignments` on every pass so stage-01 clones survive upgrade without
  duplicating rows. `jobs.user_id` has always been NOT NULL and already isolates
  outputs; `jobs` gets no ownership column. Use `db.addColumnIfMissing` and
  guarded `CREATE TABLE IF NOT EXISTS`; never rebuild the schema in place.
  Structural migration tests assert against a fresh install **and** a database
  upgraded from the older shape, because the two must not drift
  (`internal/db/multiuser_migration_test.go`).
- **Admin management is live-role-gated under `/admin/`.** The group manages
  user status/roles, access requests, and voice ownership/global visibility;
  every route re-reads the actor's role from `users`, so demotion is immediate.
  The final approved admin cannot be disabled, demoted or deleted, and an admin
  cannot self-demote. User deletion is a hard delete: the server explicitly
  removes their job rows, `voice_assignments` rows and rendered WAVs, retains
  voice rows with `owner_id=NULL`, and then deletes the account.
  Access-request decisions use `auth.AccessRequests`; voice actions use
  `voices.Store.SetGlobal`/`Assign`/`Unassign` (`/admin/voices/{id}/unassign`
  revokes one user without disturbing the card's other assignments).
  **A private card's access is many-to-many, not single-owner** —
  `voice_assignments` can (and routinely does) hold several rows for the same
  card. `server.(*Server).adminVoices` reflects that: it joins
  `voice_assignments` to list every current assignee per card
  (`web.AdminVoice.Assignees`), never the legacy `voices.owner_id` mirror
  column alone, which only ever names the most recent grant and would hide
  every earlier one still in force. The admin UI's "Add access" control is
  additive and "Revoke" is per-assignee — assigning a second user never
  displaces the first.
- **The Admin rail link is admin-only and badges pending requests.**
  `server.(*Server).navContext(r)` reads the live role, counts
  `access_requests` with `status='pending'`, and puts a `web.NavState` in the
  context the four full-page handlers render with; `navLink` in `layout.templ`
  draws "Admin (3)" when the count is positive. Keep this out of middleware —
  the rail is only drawn by a whole-page render, and middleware would run the
  count on every 2s queue poll as well.
- **The palette is exhaustive**: exactly the ten hexes in `DESIGN.md`. New
  values only ever come from `color-mix()` of two of them.
- **`/` is the studio.** `internal/web/studio.templ` renders the primary view
  (masthead spectrum, compose card with script editor + parameter fields,
  voice library, live queue table, playback "spoken line") inside the
  `AppShell` rail in `layout.templ`; `/voices` and `/queue` are the same
  components standalone. The queue fragment (`id="queue"`, `aria-live="polite"`)
  polls dedicated `GET /jobs/queue` every 2s via HTMX; compatibility `GET /jobs`
  still returns the same fragment. `POST /jobs` accepts the engine plus the
  studio's parameter fields (`seed`, `pace`, `pitch`, `expressiveness`,
  `normalize`, `output_48k`, plus `max_new_tokens`) into `jobs.model` and
  `params_json`, validated 400 on bad values. Blank engine selections default to
  MOSS and unknown engines are rejected. The server returns at most ten rows;
  this is the strict queue cap, not merely a visual crop. The queue's Length column and the player derive audio duration
  from the saved WAV's byte size (`audioDurations` in
  `internal/server/jobs.go`).
  **The queue table fits the viewport — never re-introduce a horizontal
  scroller.** It is `table-fixed w-full` with wrapping cells and icon-only row
  actions (the accessible name lives on `aria-label`); an `overflow-x` wrapper
  would be reset to the left by the 2s swap anyway.
  **A queue row is selectable and the player is not part of the polled
  fragment.** Clicking (or Enter/Space on) a row swaps
  `GET /jobs/{id}/player` into `#playback-body`, which sits outside `#queue`; the
  poll sends the selected take back (`hx-vals` → `?take=`), so the server
  re-renders the highlight on every tick. Putting the player inside `#queue`
  would restart playback every two seconds.
  **The queue's vertical scroll container lives outside the swapped fragment.**
  The `.scrollable-list max-h-[500px]` wrapper is static markup in the page
  templates (`Studio`, `QueuePage`), never inside `Queue`: `#queue` is replaced
  by `hx-swap="outerHTML"` on every 2s tick, so any element inside it that holds
  a scroll offset is recreated and the scroll snaps back to the top. Do not fix
  this with `hx-on` scroll-preservation handlers — under HTMX v4 the after-swap
  handler's `this` is the detached pre-swap element, so they silently no-op.
  **`jobs.model` records what rendered a take** (`jobs.DefaultModel` at enqueue,
  backfilled by `db.Migrate` for older rows). Queue rows and the player both read
  their model badges from that column — never from a presentation-only literal.
  Browser/API selection is limited to `jobs.DefaultModel` and `jobs.HiggsModel`;
  the store remains able to preserve explicit historical attribution. The frontend is templ + HTMX v4 (ESM from
  jsdelivr) + Alpine 3 + Tailwind v4; component CSS (badges, buttons, range,
  toggle, spoken line, alerts, empty states) lives in
  `internal/web/input.css`, copied to fidelity from `index.html`.

## Work Guidance

### Build, run, test

```sh
. scripts/env.sh                    # loads the Infisical machine identity
docker compose --profile tools build
docker compose up -d
docker compose run --rm test go test ./...
```

`scripts/run.sh` wraps the start/stop flow and the env.sh-then-up ordering that
secret injection depends on: `scripts/run.sh start` sources `env.sh`, explicitly
starts `whisper-server`, `app` and `litestream`, waits for app `/healthz` and the
Whisper model-serving root, and verifies `RUNPOD_API_KEY` landed in the app
process. `status` reports all three services and both health probes; `logs`
follows all three. `stop` / `restart` do what they say. It is the operator-facing
one command; the raw `docker compose` invocations above remain the source of
truth.

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
exclude `--tw-*` when checking palette exhaustiveness. That check is automated:
`internal/web/palette_test.go` fails if any hex outside the ten appears in the
compiled `app.css` (after stripping `--tw-*`/`@property` boilerplate) or in the
rendered HTML of any page template.

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
