# Generic IMAP Bridge

**Lightweight, SD-Card Optimised IMAP Sync Service (Go + Docker)**

A resilient IMAP bridge that streams emails from multiple source accounts into a central destination mailbox with minimal disk wear and strong runtime stability.

---

## ✨ Features

* **Low Disk Wear** — SQLite in WAL mode, in-memory deduplication cache
* **OOM Safe** — streams messages (no full buffering)
* **Read State Mirroring** — `\Seen` flag from source is mirrored to destination on sync
* **Connection Efficient** — single shared destination connection, exponential backoff with jitter on source and destination
* **Destination Retry** — failed appends retry up to 8 times with backoff; connection re-established automatically
* **Gmail OAuth2** — OAUTHBEARER for both source and destination; token refresh is automatic
* **Gmail Labels** — `X-GM-LABELS` applied after append via a dedicated long-lived label connection; queued, serialized, retried independently of message delivery
* **Source Retention** — optional per-source pruning of messages older than N days (`retention_days: 0` keeps forever)
* **Daily Digest** — error/warning report delivered as an email to a configurable label at 07:00; always arrives unread; repeated errors collapsed with a count
* **Reliable Sync** — UID-based incremental sync, Message-ID deduplication
* **Graceful Shutdown** — SIGTERM/SIGINT safe, clean connection teardown
* **Docker Ready** — non-root container, healthcheck, log rotation

---

## 📦 Quick Start

### 1. Clone & setup

```bash
git clone https://github.com/lamclennan/imap-bridge.git
cd imap-bridge
mkdir -p data tokens keys
```

### 2. Configure

```bash
cp config.example.json config.json
```

**Plain IMAP destination + source:**

```json
{
  "destination": {
    "host": "imap.destination.com:993",
    "user": "collector@domain.com",
    "pass": "app-password",
    "security": "ssl",
    "report_label": "Bridge/Reports",
    "skip_verify": false
  },
  "sources": [
    {
      "host": "imap.source.com:993",
      "user": "user@source.com",
      "pass": "password",
      "security": "ssl",
      "retention_days": 0,
      "mappings": [
        { "from": "INBOX", "to": ["INBOX"] }
      ]
    }
  ]
}
```

**Outlook.com / Hotmail** — standard IMAP with an app password (generate one at account.microsoft.com → Security → App passwords):

```json
{
  "host":     "imap-mail.outlook.com:993",
  "user":     "you@outlook.com",
  "pass":     "app-password-here",
  "security": "ssl"
}
```

**Gmail accounts** — set `"provider": "gmail"` and omit `pass`. This applies equally to source and destination Gmail accounts; each account needs its own `credentials_file` and `token_file`, but accounts within the same Google Cloud project can share a `credentials_file`:

```json
{
  "host":             "imap.gmail.com:993",
  "user":             "you@gmail.com",
  "security":         "ssl",
  "provider":         "gmail",
  "credentials_file": "keys/client_secret_you.json",
  "token_file":       "tokens/token_you.json"
}
```

### 3. Gmail OAuth2 setup (first time per account)

1. In [Google Cloud Console](https://console.cloud.google.com/), create a project, enable the **Gmail API**, create an **OAuth 2.0 Desktop** credential, and download the JSON file to `./keys/` — name it clearly, e.g. `keys/client_secret_archive.json`.
2. Run the interactive auth flow for each Gmail account:
   ```bash
   docker compose run --rm imap-bridge
   ```
3. Open the printed URL, grant access, paste the code back. The token is written to `tokens/` and refreshed automatically — you only do this once per account.

**Multiple Gmail accounts** — each account needs its own `credentials_file` and `token_file`:

```json
{ "credentials_file": "keys/client_secret_archive.json", "token_file": "tokens/token_archive.json" }
{ "credentials_file": "keys/client_secret_work.json",    "token_file": "tokens/token_work.json"    }
```

You can reuse the same `credentials_file` across multiple accounts if they share a Google Cloud project. Each account always needs its own `token_file`.

> The `keys/` folder is mounted read-only. The `tokens/` folder is writable so refreshed tokens can be persisted.

### 4. Run

```bash
docker compose up -d
docker logs -f imap-bridge
```

On startup you will see a confirmation line for each successful connection, for example:

```
source connected: imap.gmail.com:993 (you@gmail.com)
```

A successful destination connection is confirmed the first time it delivers a message or on the initial folder-discovery pass. If credentials are wrong the bridge will log the error and retry with backoff.

---

## ⚙️ Config reference

| Field | Scope | Description |
|---|---|---|
| `debug` | top-level | `true` to enable verbose per-message logging; default `false` |
| `host` | source / dest | `hostname:port` |
| `user` | source / dest | IMAP username / email address |
| `pass` | source / dest | Password or app password (omit for Gmail OAuth2) |
| `security` | source / dest | `ssl` (port 993), `tls` (STARTTLS), `""` (plain) |
| `skip_verify` | source / dest | Skip TLS cert check — dev/testing only |
| `provider` | source / dest | `"gmail"` to use OAuth2 instead of password |
| `credentials_file` | source / dest | Path to Google OAuth2 `client_secret.json` |
| `token_file` | source / dest | Path to cached OAuth2 token (unique per account) |
| `report_label` | dest only | Folder for daily error digest; empty = disabled |
| `retention_days` | source only | `-1` = sync full history, never prune (eligible for `sync_new_only`); `0` = keep forever (eligible for `sync_new_only`); `>0` = prune source messages older than N days |
| `disable_idle` | source only | `true` to disable IMAP IDLE and use polling only; use when server IDLE is broken |
| `poll_interval` | source only | Poll interval in seconds when IDLE is disabled or falls back; default `600` (10 minutes) |
| `sync_new_only` | source only | On first run, skip all existing mail and only sync new messages arriving after startup. Only applies when `retention_days` is `0` or `-1`. |
| `max_error_retention_days` | source only | Stop retrying permanently skipped messages after N days; `0` = retry forever. Skipped messages appear in the daily report until expired or resolved. |
| `mappings` | source only | `[{ "from": "...", "to": ["dest1", "dest2"], "labels": ["tag"] }]` — `to` is an array; `labels` is optional — see below |

---

## 📬 Multiple destinations & labels

The `to` field is an array — list as many destination folders as you need. The message body is fetched from the source once. If there is only one destination it is streamed directly with no buffering. If there are multiple destinations the body is read into memory once and replayed, which is unavoidable since an `io.Reader` can only be consumed once.

The optional `labels` array behaviour depends on the destination provider:

- **Gmail** (`"provider": "gmail"`) — labels are applied via `X-GM-LABELS STORE` after each append, processed by a dedicated long-lived label connection. STOREs are queued and serialized; failures retry with the same backoff as the append connection and are logged to the daily digest. The message is always delivered even if the label store ultimately fails.
- **Standard IMAP** (Dovecot, Courier, OVH, cPanel, etc.) — labels are set as IMAP keyword flags on the `APPEND` command itself. Support is server-dependent; most modern servers accept keywords but they appear as flags rather than visible label folders. If your server does not support keywords the flags are silently ignored — message delivery is unaffected.

```json
"mappings": [
  {
    "from": "INBOX",
    "to":   ["INBOX", "Bridged/OVH"],
    "labels": ["bridged"]
  },
  {
    "from": "INBOX/Work",
    "to":   ["INBOX", "Work"],
    "labels": ["bridged", "work"]
  }
]
```

> **Gmail note:** Gmail labels are also IMAP folders. To deliver to both inbox and a label folder, list both in `to`. The `labels` field applies Gmail labels (tags) on top of that. Gmail filters do not fire on IMAP-appended messages.

Deduplication is tracked per `(message-id, destination-folder)` pair so the same message can be appended to multiple folders correctly. If one destination fails it is logged and retried on the next sync — other destinations for the same message are unaffected.

---

## ⚠️ Gmail IMAP limits — this is not a migration tool

Gmail enforces IMAP bandwidth limits per account:

- **~2,500 MB per day** download limit for IMAP clients
- **~500 MB per day** upload limit via IMAP APPEND
- Exceeding limits results in temporary `[OVERQUOTA]` or authentication errors for several hours

This bridge is designed for **continuous low-volume mirroring** of ongoing mail — not bulk import of historical mailboxes. If you have a large existing mailbox to migrate, use a dedicated migration tool first, then enable this bridge for ongoing sync going forward.

Use `sync_new_only: true` when setting up a new source with an existing inbox to avoid triggering rate limits on first run.

---

If a message fails to sync after `5` consecutive attempts it is permanently skipped so subsequent mail is not blocked. The daily report will include the skipped message every day until it is resolved.

**Automatic daily retry:** At 07:00 each day the bridge resets skipped messages so they are retried. If the message was deleted from the source mailbox, the next sync will find it gone and the failure record is cleared automatically. No manual intervention needed in the normal case.

**`max_error_retention_days`:** After this many days the failure record is deleted and the message will no longer be retried or shown in reports. Useful for messages that are permanently undeliverable.

**Manual clear via sqlite3** (if needed):
```bash
# View all skipped messages
sqlite3 data/state.db "SELECT key, skipped_at, last_error FROM sync_failures WHERE skipped_at IS NOT NULL;"

# Clear all — retry everything on next sync
sqlite3 data/state.db "DELETE FROM sync_failures;"

# Clear one specific message
sqlite3 data/state.db "DELETE FROM sync_failures WHERE key='user@example.com:INBOX:3605';"
```

---

If you're unsure of the exact folder names on a source or destination server,
leave `mappings` as an empty array for that source in `config.json`:

```json
{
  "host": "imap.example.com:993",
  "user": "user@example.com",
  "pass": "password",
  "security": "ssl",
  "mappings": []
}
```

On startup, the bridge will detect the missing mappings, print the full folder
list for every unmapped source **and** for the destination, then exit cleanly.
Read the output with:

```bash
docker logs imap-bridge
```

Example output:

```
folder-discovery: one or more sources have no mappings — listing folders and exiting
folder-discovery: destination (archive@gmail.com) — 6 folder(s):
  folder-discovery:   INBOX
  folder-discovery:   [Gmail]/All Mail
  folder-discovery:   [Gmail]/Sent Mail
  folder-discovery:   [Gmail]/Trash
  folder-discovery:   Bridge/Reports
  folder-discovery:   Work
folder-discovery: source[0] (user@example.com) — 4 folder(s):
  folder-discovery:   INBOX
  folder-discovery:   INBOX/Sent
  folder-discovery:   INBOX/Work
  folder-discovery:   INBOX/Drafts
folder-discovery: add mappings to config.json and restart
```

Once you've identified the folder names, add your `mappings` and restart.

---

## 🔐 Security tips

* Use **app passwords** for non-Gmail IMAP accounts
* Use **OAuth2** for Gmail — Google blocks plain IMAP auth for most accounts
* Set `chmod 600` on `client_secret.json` and all token files
* Avoid `skip_verify: true` in production
* Consider Docker secrets for `pass` values

---

## ⚙️ How it works

1. Connects to each source IMAP account (password or OAuth2); logs a confirmation line on success
2. Tracks the last seen UID per folder in SQLite
3. On new mail (IMAP IDLE or configurable poll interval), fetches new messages
4. Mirrors `\Seen` flag from source — read mail stays read on the destination
5. Appends to every destination folder listed in `to`; retries with backoff on failure
6. If destination is Gmail and `labels` are configured, enqueues a `X-GM-LABELS STORE` to a dedicated long-lived label connection that serializes and retries label application independently of delivery
7. Optionally prunes source messages older than `retention_days` after each sync
8. Delivers a daily digest email at 07:00 with any errors or warnings (collapsed duplicates)

---

## 🐳 Docker notes

* SQLite state in `./data/state.db` (persistent volume)
* `config.json` mounted read-only from project root
* `./keys/` mounted read-only — place all `client_secret_*.json` files here
* `./tokens/` mounted writable — one token file per Gmail account, auto-refreshed
* Log rotation: 10 MB × 3 files
* Non-root user inside container

---

## 🚧 Limitations

* Relies on Message-ID for deduplication — not always guaranteed unique
* OAuth2 first-run requires terminal access to the host
* SQLite limits horizontal scaling

---

## 🛣️ Roadmap ideas

* [ ] Disk-backed retry queue for exhausted append attempts
* [ ] Prometheus metrics endpoint
* [ ] Config hot reload (SIGHUP)
* [ ] Message hashing fallback for missing Message-ID
* [ ] Non-interactive OAuth2 device flow

---

## 📄 License

MIT
