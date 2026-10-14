# YourSix IFTTT

A small Go service that receives [YourSix event webhooks](https://docs.yoursix.com/reference/events/intro) over a Cloudflare Tunnel and runs programmable actions on your local network. For example, a motion event on one camera can pulse an output on another camera, move a PTZ to a preset, call your home automation, or post to Discord.

- **Public:** `POST /hook/<token>` (the webhook receiver) and `GET /healthz`.
- **Behind Discord login:** the web UI for editing rules, a live event and run log (pushed from the server as things happen, no polling), testing rules, and simulating events.
- **Storage:** SQLite in a Docker volume. The image is a single static binary on distroless.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/rules-dark.png">
  <img alt="Rules list showing triggers, actions, cooldown and one-at-a-time badges" src="docs/screenshots/rules-light.png">
</picture>

## Screenshots

**Event log.** Every webhook is recorded with the rules it triggered, and new events and rule results appear live. Expand one to see each action's result. **Matrix ↗** opens the event's camera in the YourSix Matrix, playing back from when it happened.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/events-dark.png">
  <img alt="Event log with an expanded run showing each action's HTTP status and response" src="docs/screenshots/events-light.png">
</picture>

**Rule editor.** Pick topics and event codes, add filters, and build the action list. You can also switch to raw JSON.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/editor-dark.png">
  <img alt="Rule editor with cooldown settings, event code pickers, filters and an HTTP action using digest auth" src="docs/screenshots/editor-light.png">
</picture>

**Secrets.** Encrypted, write-only, and private to each admin.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/secrets-dark.png">
  <img alt="Secrets tab listing secret names and the rules that use them, with no values shown" src="docs/screenshots/secrets-light.png">
</picture>

**Setup.** Shows the webhook URL to paste into YourSix, and lets you simulate any event.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/setup-dark.png">
  <img alt="Setup tab showing the webhook URL and the event simulator" src="docs/screenshots/setup-light.png">
</picture>


## How rules work

A rule has an **If** part, a list of **Then** actions, and an optional cooldown.

**If:**
- **Topics** (`device`, `alarmgroup`, `barrier`, `peripheral`, `sensor`). Empty means any topic.
- **Event codes** (`vmd`, `aoa`, `input_activated`, …), either bare or qualified like `device:vmd`. Empty means any code.
- **Filters** on any `eventData` key (`deviceName`, `siteName`, `groupState`, `videoSourceId`, …) or top-level field (`eventCode`, …), using `equals`, `not_equals`, `contains`, `starts_with`, `regex`, `exists` or `not_exists`. Every filter must match.

**Then**, run in order:

| Type | What it does |
|---|---|
| `http` | Any method, URL, headers and body. Auth can be none, `basic`, or `digest` (Axis VAPIX default). Optional self-signed TLS, custom expected status codes, and a timeout. |
| `delay` | Waits N ms between steps. |
| `discord` | Posts a message to a Discord webhook. |
| `request` | Runs a **saved request** (see below) with values for its parameters. |

By default a failing action stops the rule. Set `continueOnError` on an action to keep going after it fails.

While a rule runs, the UI shows it live:

- The rule's card on the **Rules** tab is highlighted and reads, e.g., *Step 2 of 4 · Settle · 4.2s left*. Its finished steps are ticked and the current one is marked.
- The event's badge in the **Events** log shows, e.g., *2/4 · 4.2s*.
- **Delay** steps count down with a progress bar; other steps show elapsed time.

URL, body, header values, password and message are Go templates:

```
{{ .data.deviceName }}  {{ .data.siteName }}  {{ .event.eventCode }}  {{ .event.key }}  {{ .rule }}
{{ json .data.deviceName }}        -> "CAM-01"  (safe inside JSON bodies)
{{ urlquery .data.deviceName }}    -> URL-escaped
{{ secret "CAMERA_PASS" }}         -> your secret CAMERA_PASS (Secrets tab)
```

Rules can be edited in the form editor or as raw JSON (toggle in the editor). See [testdata/rules/motion-to-mock-camera.json](testdata/rules/motion-to-mock-camera.json) for an example.

### Saved requests

The **Requests** tab holds reusable HTTP requests, so the same camera call doesn't have to be copied into every rule. Each one has a name, an HTTP request (method, URL, auth, headers, body, timeout), and optional **parameters**. Its templates use parameters as `{{ .params.NAME }}`, alongside the usual event data.

Rules use one with the **+ Saved request** action. Pick the request, then fill in its parameters; values can use event data, e.g. `preset = {{ .data.deviceName }}-view`. A parameter with a default is optional, and one without a default is required. Editing a saved request updates every rule that uses it. A request can't be deleted while rules still use it. Each request has its own **Test** button, which asks for parameter values first.

**Saved requests and secrets.** Parameters can change where a request goes, for example a `host` parameter in the URL. So:

- A saved request that uses secrets runs with the secrets of the admin who last saved it, and **only that admin's rules can use it**. This is checked when a rule is saved and again on every run.
- Saved requests without secrets can be used by anyone.
- Parameter values can't use `{{ secret }}`. Put the secret in the saved request itself.

### Secrets

The **Secrets** tab (admins only) stores credentials such as camera passwords:

- **Encrypted at rest:** values are stored with AES-256-GCM using `Y6IFTTT_SECRETS_KEY`, which lives only in `.env`. Each value is bound to its owner and name, so a database row copied to another user or renamed won't decrypt. Without the key, the database (or a backup of it) reveals nothing.
- **Write-only:** the API never returns a value, not even to its owner. You can only replace or delete it.
- **Private to the admin who added it:** other admins can't list, use, replace or delete your secrets.
- **Usable only by your rules:** a rule runs with the secrets of the admin who last saved it. Saving a rule that references a secret you don't own is rejected. Another admin therefore can't edit your rule to send your password somewhere else; they'd have to save it, which makes it theirs, without your secrets. Unchanged rules still run as their owner, including via **Test** and replay.
- **Literal names only:** names must be quoted literals (`{{ secret "NAME" }}`), so a template can't probe for names dynamically.
- **Masked in history:** values used in a run are masked (`••••••`) in the run history if they show up in a URL, response or error. Values shorter than 4 characters aren't masked.

`Y6IFTTT_SECRET_*` environment variables still work as operator-wide secrets that any admin's rules can use. A stored secret with the same name takes precedence. Without `Y6IFTTT_SECRETS_KEY`, the Secrets tab is disabled and only env secrets work.

**Key handling:** if you lose or change the key, stored secrets can't be decrypted and must be re-entered. Back up `.env` separately from the data volume.

### Open in Matrix

Each event in the log has a **Matrix ↗** button, and its expanded view has **Open in Matrix ↗**. Both use YourSix's [Matrix deeplinks](https://docs.yoursix.com/reference/integration) to open the event's video, playing back from the moment it **occurred**:

- Camera events with a `videoSourceId` open that stream: `deviceIds=<deviceId>_<videoSourceId + 1>` (events count sources from 0, the Matrix from 1).
- Other device events open the whole device: `deviceIds=<deviceId>`.
- Alarm group events open the group: `alarmGroupId=<groupId>`.

The timestamp is in seconds with milliseconds, for example `1791996197.084`. Set `Y6IFTTT_YOURSIX_PLATFORM_URL` if your organization uses a different YourSix web address.

### Ignoring events (rate limiting)

Each rule can accept an event and log it without acting on it:

- **Ignore while running** (`"skipIfRunning": true`): if a run of this rule is still in progress, the event is logged as `busy` and nothing happens. Without this option, overlapping runs execute in parallel.
- **Run at most once every N seconds** (`"cooldownSeconds": N`): events inside the window are logged as `cooldown`. The window can start when the last run **started** (`"cooldownFrom": "start"`, the default) or when it **finished** (`"cooldownFrom": "finish"`). With `finish`, events that arrive while a run is still going are ignored too, because the window hasn't opened yet.

The two options can be combined. Ignored events still appear in the Events tab with the reason. The rule's **Test** button skips both checks.

Duplicate deliveries (same `eventId`) are recorded once and never fire twice.

## Test environment

You need Docker and [Task](https://taskfile.dev). Discord and Cloudflare aren't needed.

```bash
task test:up
```

This starts the following at http://localhost:8088:
- **The app**, with dev auth: every local request is an admin. Dev auth refuses any request that came through Cloudflare.
- **A mock camera** at `http://mock-camera:9090` inside the network. `/digest/*` needs digest auth `root`/`pass`, `/basic/*` needs basic auth, and everything else is open.
- **One example rule**, seeded from `testdata/rules/`.

```bash
task test:fire                                   # send testdata/events/device-vmd.json
task test:fire EVENT=peripheral:input_activated  # or any topic:code sample
task test:camera                                 # see what the mock camera received
task test:down                                   # stop (task test:clean also wipes data)
```

The UI's Setup tab can also simulate any event, and each rule has a **Test** button that runs its actions with a sample event.

To run without Docker, use `task run` (dev auth on :8080). Use `task test` for the unit tests.

## Production

1. **Discord app:** in https://discord.com/developers/applications, go to OAuth2 and add the redirect `https://<your-host>/auth/callback`.
2. **Cloudflare Tunnel:** in Zero Trust, go to Networks → Tunnels and create a tunnel. Add a public hostname pointing to `http://app:8080`, then copy the tunnel token.
   - Don't put Cloudflare Access in front of `/hook/*`, because YourSix can't log in. Protecting the rest with Access is optional, since the app has its own Discord login.
3. **Configure:** copy `.env.example` to `.env` and fill it in. Generate the webhook token and session secret with `openssl rand -hex 32`.
4. **Start:** `task up`. This runs `docker compose pull && up -d` for the app and cloudflared. No ports are published on the host.
5. **YourSix:** create an organization webhook to `https://<your-host>/hook/<Y6IFTTT_WEBHOOK_TOKEN>`. The UI's Setup tab shows this URL. Then add the webhook as a recipient in a notification rule, choosing which events to send.

The app container must be able to reach your cameras. The default bridge network can reach LAN IPs through the host. If you need mDNS or broadcast, switch to `network_mode: host`.

### Webhook security

YourSix webhooks carry no signature. The documentation only gives the `y6-platform` User-Agent and the source IPs from the A record of `webhook.cloud.yoursix.com`. This service layers the available checks:

1. An unguessable token in the URL path (required, at least 24 chars). A wrong token gets a 404.
2. A User-Agent check (`Y6IFTTT_WEBHOOK_REQUIRE_USER_AGENT`, on by default).
3. A source IP check against the resolved A record, using `CF-Connecting-IP` behind the tunnel (`Y6IFTTT_WEBHOOK_VERIFY_SOURCE_IP`).
4. A body size limit, plus dedupe on `eventId`.

The UI uses Discord OAuth (`identify` scope only) with HMAC-signed session cookies. Roles come from `Y6IFTTT_ADMIN_USER_IDS` and `Y6IFTTT_VIEWER_USER_IDS` and are checked on every request, so removing an ID revokes access. State-changing requests are protected against cross-site requests with Go's `http.CrossOriginProtection`, and a strict CSP is set.

## Releasing

Publish a GitHub release tagged `vX.Y.Z`. The [release workflow](.github/workflows/release.yml) tests the code, builds `linux/amd64` and `linux/arm64` images, and pushes them to `ghcr.io/<owner>/<repo>` as `X.Y.Z`, `X.Y`, `X` and `latest`. Pre-releases get only their exact tag. CI also runs on every push and pull request.

## Configuration reference

All settings are environment variables prefixed `Y6IFTTT_`. See [.env.example](.env.example) for the full list.

| Variable | Default | Purpose |
|---|---|---|
| `ADDR` | `:8080` | Listen address |
| `DATA_DIR` | `data` | SQLite location |
| `PUBLIC_URL` | | External base URL, also used to derive the OAuth redirect |
| `WEBHOOK_TOKEN` | required | Secret path segment of the webhook URL |
| `WEBHOOK_REQUIRE_USER_AGENT` | `true` | Require `y6-platform` User-Agent |
| `WEBHOOK_VERIFY_SOURCE_IP` | `false` | Require a YourSix source IP |
| `CLIENT_IP_HEADER` | | Trusted header for the client IP (`CF-Connecting-IP`) |
| `DISCORD_CLIENT_ID` / `_SECRET` | required | Discord OAuth app |
| `SESSION_SECRET` | required | At least 32 chars, signs session cookies |
| `ADMIN_USER_IDS` / `VIEWER_USER_IDS` | | Allowed Discord user IDs |
| `DISCORD_WEBHOOK_URL` | | Default target for Discord message actions |
| `SECRET_*` | | Values exposed to templates via `{{ secret "*" }}` |
| `EVENT_RETENTION_DAYS` | `30` | Event and run history retention |
| `SECRETS_KEY` | | 32-byte key (hex/base64) enabling the encrypted Secrets tab |
| `YOURSIX_PLATFORM_URL` | `https://platform.yoursix.com` | YourSix web app for "Open in Matrix" links |
| `SEED_RULES_DIR` | | Import `*.json` rules on first start (empty DB only) |
| `DEV_AUTH` | `false` | Local testing only: skip Discord login |
