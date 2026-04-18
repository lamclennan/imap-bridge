# Generic IMAP Bridge

**Lightweight, SD-Card Optimised IMAP Sync Service (Go + Docker)**

A resilient IMAP bridge that streams emails from multiple source accounts into a central destination mailbox with minimal disk wear and strong runtime stability.

---

## ✨ Features

* **Low Disk Wear** — SQLite in WAL mode, in-memory deduplication cache
* **OOM Safe** — streams messages (no full buffering)
* **Connection Efficient** — reuses IMAP connections, exponential backoff with jitter on both source and destination
* **Destination Retry** — failed appends retry up to 8 times with backoff; connection is re-established automatically
* **Gmail OAuth2** — XOAUTH2 support for both source and destination Gmail accounts; token refresh is automatic
* **Reliable Sync** — UID-based incremental sync, Message-ID deduplication
* **Graceful Shutdown** — SIGTERM/SIGINT safe, clean connection teardown
* **Docker Ready** — non-root container, healthcheck included

---

## 📦 Quick Start

### 1. Clone & setup

```bash
git clone https://github.com/lamclennan/imap-bridge.git
cd imap-bridge
mkdir data
```

### 2. Configure

Copy the example and edit it:

```bash
cp config.example.json config.json
```

**Plain IMAP** — just fill in `host`, `user`, `pass`, `security`:

```json
{
  "destination": {
    "host": "imap.destination.com:993",
    "user": "collector@domain.com",
    "pass": "app-password",
    "security": "ssl",
    "mark_as_read": true,
    "skip_verify": false,
    "report_label": "INBOX"
  },
  "sources": [
    {
      "host": "imap.source.com:993",
      "user": "user@source.com",
      "pass": "password",
      "security": "ssl",
      "retention_days": 90,
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
  "token_file":       "token_you.json"
}
```

See `config.example.json` for a full example with all fields.

### 3. Gmail OAuth2 setup (first time only)

1. In [Google Cloud Console](https://console.cloud.google.com/), create a project, enable the **Gmail API**, and create an **OAuth 2.0 Desktop** client credential.
2. Download the credential as `client_secret.json` and place it alongside `config.json`.
3. On first run, the bridge prints an authorisation URL. Open it in a browser, grant access, and paste the code back into the terminal.
4. The token is cached to `token_file` and refreshed automatically — you only do this once per account.

> Each Gmail account (source or destination) needs its own `token_file` path.

### 4. Run with Docker

```bash
docker compose up -d
```

### 5. View logs

```bash
docker logs -f imap-bridge
```

---

## 🐳 Docker notes

* Data is stored in `./data/state.db`
* Place `client_secret.json` and token files in the project root (mounted into the container)
* WAL mode enabled for SD-card longevity
* Container runs as non-root user

---

## 🔐 Security tips

* Use **app passwords** for non-Gmail IMAP accounts
* Use **OAuth2** (not passwords) for Gmail — Google blocks plain IMAP auth for most accounts
* Avoid `skip_verify: true` in production
* Set `chmod 600` on `client_secret.json` and token files
* Consider Docker secrets for sensitive values

---

## ⚙️ How it works

1. Connects to each source IMAP account (password or OAuth2)
2. Tracks the last seen UID per folder in SQLite
3. On new mail (IMAP IDLE or 10-minute poll), streams new messages to the destination
4. Deduplicates using Message-ID — in-memory cache backed by SQLite
5. Destination appends are retried with backoff on any connection or server error

---

## ⚙️ Config reference

| Field | Where | Description |
|---|---|---|
| `host` | source / dest | `hostname:port` |
| `user` | source / dest | IMAP username / email address |
| `pass` | source / dest | Password or app password (omit for Gmail) |
| `security` | source / dest | `ssl` (port 993), `tls` (STARTTLS), or `""` (plain) |
| `skip_verify` | source / dest | Skip TLS certificate check (dev only) |
| `provider` | source / dest | `"gmail"` to use OAuth2 instead of password |
| `credentials_file` | source / dest | Path to Google OAuth2 `client_secret.json` |
| `token_file` | source / dest | Path to cached OAuth2 token (unique per account) |
| `mark_as_read` | dest only | Mark appended messages as read |
| `report_label` | dest only | Default destination folder |
| `retention_days` | source only | Reserved for future pruning logic |
| `mappings` | source only | Array of `{ "from": "...", "to": "..." }` folder mappings |

---

## 🚧 Limitations

* Relies on Message-ID (not always guaranteed unique)
* SQLite limits horizontal scaling
* OAuth2 first-run authorisation requires terminal access to the host

---

## 🛣️ Roadmap ideas

* [ ] Disk-backed retry queue
* [ ] Prometheus metrics
* [ ] Config hot reload (SIGHUP)
* [ ] Message hashing fallback for missing Message-ID
* [ ] Non-interactive OAuth2 flow (service account / device flow)

---

## 📄 License

MIT
