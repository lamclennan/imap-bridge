# Generic IMAP Bridge

**Lightweight, SD-Card Optimised IMAP Sync Service (Go + Docker)**

A resilient IMAP bridge that streams emails from multiple source accounts into a central destination mailbox with minimal disk wear and strong runtime stability.

\---

## ✨ Features

* **Low Disk Wear** — SQLite in WAL mode, in-memory deduplication cache
* **OOM Safe** — streams messages (no full buffering)
* **Read State Mirroring** — `\\Seen` flag from source is mirrored to destination on sync
* **Connection Efficient** — single shared destination connection, exponential backoff with jitter on source and destination
* **Destination Retry** — failed appends retry up to 8 times with backoff; connection re-established automatically
* **Gmail OAuth2** — XOAUTH2 for both source and destination; token refresh is automatic
* **Source Retention** — optional per-source pruning of messages older than N days (`retention\_days: 0` keeps forever)
* **Daily Digest** — error/warning report delivered as an email to a configurable label at 07:00; always arrives unread; repeated errors collapsed with a count
* **Reliable Sync** — UID-based incremental sync, Message-ID deduplication
* **Graceful Shutdown** — SIGTERM/SIGINT safe, clean connection teardown
* **Docker Ready** — non-root container, healthcheck, log rotation

\---

## 📦 Quick Start

### 1\. Clone \& setup

```bash
git clone https://github.com/lamclennan/imap-bridge.git
cd imap-bridge
mkdir -p data tokens
```

### 2\. Configure

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
    "report\_label": "Bridge/Reports",
    "skip\_verify": false
  },
  "sources": \[
    {
      "host": "imap.source.com:993",
      "user": "user@source.com",
      "pass": "password",
      "security": "ssl",
      "retention\_days": 0,
      "mappings": \[
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
  "credentials\_file": "client\_secret.json",
  "token\_file":       "tokens/token\_you.json"
}
```

### 3\. Gmail OAuth2 setup (first time only)

1. In [Google Cloud Console](https://console.cloud.google.com/), create a project, enable the **Gmail API**, create an **OAuth 2.0 Desktop** credential, and download `client\_secret.json` to the project root.
2. Run the interactive auth flow:

```bash
   docker compose run --rm imap-bridge
   ```

3. Open the printed URL, grant access, paste the code back. Token is written to `tokens/` and refreshed automatically — you only do this once per account.

> Each Gmail account needs its own `token\_file` path.

### 4\. Run

```bash
docker compose up -d
docker logs -f imap-bridge
```

\---

## ⚙️ Config reference

|Field|Scope|Description|
|-|-|-|
|`host`|source / dest|`hostname:port`|
|`user`|source / dest|IMAP username / email address|
|`pass`|source / dest|Password or app password (omit for Gmail)|
|`security`|source / dest|`ssl` (port 993), `tls` (STARTTLS), `""` (plain)|
|`skip\_verify`|source / dest|Skip TLS cert check — dev/testing only|
|`provider`|source / dest|`"gmail"` to use OAuth2 instead of password|
|`credentials\_file`|source / dest|Path to Google OAuth2 `client\_secret.json`|
|`token\_file`|source / dest|Path to cached OAuth2 token (unique per account)|
|`report\_label`|dest only|Folder for daily error digest; empty = disabled|
|`retention\_days`|source only|Prune source messages older than N days; `0` = keep forever|
|`mappings`|source only|`\[{ "from": "...", "to": "...", "labels": \["tag"] }]` folder pairs; `labels` is optional — see *Mapping labels* below|

\---

## 🏷️ Mapping labels

Each mapping can include an optional `labels` array. The strings are set as
IMAP keywords (flags) on every message appended to the destination folder.
On servers that support them as user-visible tags (e.g. Dovecot with
`autocreate\_keywords = yes`, or Gmail where keywords map to labels) this
lets you tag bridged mail automatically.

```json
"mappings": \[
  { "from": "INBOX",      "to": "INBOX",   "labels": \["bridged"] },
  { "from": "INBOX/Work", "to": "Work",    "labels": \["bridged", "work-src"] }
]
```

> \*\*Note:\*\* IMAP keywords are server-dependent. Gmail ignores arbitrary
> keywords on APPEND; standard IMAP servers (Dovecot, Courier, etc.) support
> them. Labels that the server does not accept are silently dropped by most
> implementations — check your server's docs if tags are not appearing.
> The built-in `\\Seen` flag mirroring is unaffected by this field.

\---

## 🗂️ Discovering folder names

If you're unsure of the exact folder names on a source or destination server,
leave `mappings` as an empty array for that source in `config.json`:

```json
{
  "host": "imap.example.com:993",
  "user": "user@example.com",
  "pass": "password",
  "security": "ssl",
  "mappings": \[]
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
  folder-discovery:   \[Gmail]/All Mail
  folder-discovery:   \[Gmail]/Sent Mail
  folder-discovery:   \[Gmail]/Trash
  folder-discovery:   Bridge/Reports
  folder-discovery:   Work
folder-discovery: source\[0] (user@example.com) — 4 folder(s):
  folder-discovery:   INBOX
  folder-discovery:   INBOX/Sent
  folder-discovery:   INBOX/Work
  folder-discovery:   INBOX/Drafts
folder-discovery: add mappings to config.json and restart
```

Once you've identified the folder names, add your `mappings` and restart.

\---

* Use **app passwords** for non-Gmail IMAP accounts
* Use **OAuth2** for Gmail — Google blocks plain IMAP auth for most accounts
* Set `chmod 600` on `client\_secret.json` and all token files
* Avoid `skip\_verify: true` in production
* Consider Docker secrets for `pass` values

\---

## ⚙️ How it works

1. Connects to each source IMAP account (password or OAuth2)
2. Tracks the last seen UID per folder in SQLite
3. On new mail (IMAP IDLE or 10-minute poll), fetches new messages
4. Mirrors `\\Seen` flag from source — read mail stays read on the destination
5. Appends to the mapped destination folder; retries with backoff on failure
6. Optionally prunes source messages older than `retention\_days` after each sync
7. Delivers a daily digest email at 07:00 with any errors or warnings (collapsed duplicates)

\---

## 🐳 Docker notes

* SQLite state in `./data/state.db` (persistent volume)
* `config.json`, `client\_secret.json`, and `tokens/` mounted from project root
* Log rotation: 10 MB × 3 files
* Non-root user inside container

\---

## 🚧 Limitations

* Relies on Message-ID for deduplication — not always guaranteed unique
* OAuth2 first-run requires terminal access to the host
* SQLite limits horizontal scaling

\---

## 🛣️ Roadmap ideas

* \[ ] Disk-backed retry queue for exhausted append attempts
* \[ ] Prometheus metrics endpoint
* \[ ] Config hot reload (SIGHUP)
* \[ ] Message hashing fallback for missing Message-ID
* \[ ] Non-interactive OAuth2 device flow

\---

## 📄 License

MIT

