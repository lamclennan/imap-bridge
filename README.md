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
* **Gmail OAuth2** — XOAUTH2 for both source and destination; token refresh is automatic
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
mkdir -p data tokens
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
        { "from": "INBOX", "to": "INBOX" }
      ]
    }
  ]
}
```

**Gmail accounts** — set `"provider": "gmail"` and omit `pass`:

```json
{
  "host":             "imap.gmail.com:993",
  "user":             "you@gmail.com",
  "security":         "ssl",
  "provider":         "gmail",
  "credentials_file": "client_secret.json",
  "token_file":       "tokens/token_you.json"
}
```

### 3. Gmail OAuth2 setup (first time only)

1. In [Google Cloud Console](https://console.cloud.google.com/), create a project, enable the **Gmail API**, create an **OAuth 2.0 Desktop** credential, and download `client_secret.json` to the project root.
2. Run the interactive auth flow:
   ```bash
   docker compose run --rm imap-bridge
   ```
3. Open the printed URL, grant access, paste the code back. Token is written to `tokens/` and refreshed automatically — you only do this once per account.

> Each Gmail account needs its own `token_file` path.

### 4. Run

```bash
docker compose up -d
docker logs -f imap-bridge
```

---

## ⚙️ Config reference

| Field | Scope | Description |
|---|---|---|
| `host` | source / dest | `hostname:port` |
| `user` | source / dest | IMAP username / email address |
| `pass` | source / dest | Password or app password (omit for Gmail) |
| `security` | source / dest | `ssl` (port 993), `tls` (STARTTLS), `""` (plain) |
| `skip_verify` | source / dest | Skip TLS cert check — dev/testing only |
| `provider` | source / dest | `"gmail"` to use OAuth2 instead of password |
| `credentials_file` | source / dest | Path to Google OAuth2 `client_secret.json` |
| `token_file` | source / dest | Path to cached OAuth2 token (unique per account) |
| `report_label` | dest only | Folder for daily error digest; empty = disabled |
| `retention_days` | source only | Prune source messages older than N days; `0` = keep forever |
| `mappings` | source only | `[{ "from": "...", "to": "..." }]` folder pairs |

---

## 🔐 Security tips

* Use **app passwords** for non-Gmail IMAP accounts
* Use **OAuth2** for Gmail — Google blocks plain IMAP auth for most accounts
* Set `chmod 600` on `client_secret.json` and all token files
* Avoid `skip_verify: true` in production
* Consider Docker secrets for `pass` values

---

## ⚙️ How it works

1. Connects to each source IMAP account (password or OAuth2)
2. Tracks the last seen UID per folder in SQLite
3. On new mail (IMAP IDLE or 10-minute poll), fetches new messages
4. Mirrors `\Seen` flag from source — read mail stays read on the destination
5. Appends to the mapped destination folder; retries with backoff on failure
6. Optionally prunes source messages older than `retention_days` after each sync
7. Delivers a daily digest email at 07:00 with any errors or warnings (collapsed duplicates)

---

## 🐳 Docker notes

* SQLite state in `./data/state.db` (persistent volume)
* `config.json`, `client_secret.json`, and `tokens/` mounted from project root
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
