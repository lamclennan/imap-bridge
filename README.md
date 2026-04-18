# Generic IMAP Bridge

**Lightweight, SD-Card Optimized IMAP Sync Service (Go + Docker)**

A resilient IMAP bridge that streams emails from multiple source accounts into a central destination mailbox with minimal disk wear and strong runtime stability.

---

## ✨ Features

* **Low Disk Wear**

  * SQLite in WAL mode
  * In-memory deduplication cache

* **OOM Safe**

  * Streams messages (no full buffering)

* **Connection Efficient**

  * Reuses IMAP connections
  * Exponential backoff with jitter

* **Reliable Sync**

  * UID-based incremental sync
  * Message-ID deduplication

* **Graceful Shutdown**

  * SIGTERM/SIGINT safe
  * Clean connection teardown

* **Docker Ready**

  * Non-root container
  * Healthcheck included

---

## 📦 Quick Start

### 1. Clone & Setup

```bash
git clone https://github.com/YOUR_USERNAME/imap-bridge.git
cd imap-bridge
mkdir data
```

---

### 2. Configure

Edit `config.json`:

```json
{
  "destination": {
    "host": "imap.destination.com:993",
    "user": "collector@domain.com",
    "pass": "app-password",
    "security": "ssl",
    "mark_as_read": true,
    "skip_verify": false,
    "report_label": "INBOX/Reports"
  },
  "sources": [
    {
      "host": "imap.source.com:993",
      "user": "user@source.com",
      "pass": "password",
      "security": "ssl",
      "retention_days": 0,
      "mappings": [
        { "from": "INBOX", "to": "Archive/Inbox" }
      ]
    }
  ]
}
```

---

### 3. Run with Docker

```bash
docker compose up -d
```

---

### 4. View Logs

```bash
docker logs -f imap-bridge
```

---

## 🐳 Docker Notes

* Data is stored in `./data/state.db`
* WAL mode enabled for SD-card longevity
* Container runs as non-root user

---

## 🔐 Security Tips

* Use **app passwords**, not real credentials
* Avoid `skip_verify=true` in production
* Consider Docker secrets for sensitive values

---

## ⚙️ How It Works

1. Connects to source IMAP accounts
2. Tracks last UID per folder
3. Streams new messages to destination
4. Deduplicates using Message-ID
5. Stores minimal sync state in SQLite

---

## 🚧 Limitations

* Relies on Message-ID (not always guaranteed unique)
* SQLite limits horizontal scaling
* No persistent retry queue (yet)

---

## 🛣️ Roadmap Ideas

* [ ] Disk-backed retry queue
* [ ] Prometheus metrics
* [ ] Config hot reload (SIGHUP)
* [ ] Message hashing fallback

---

## 📄 License

MIT
