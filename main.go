package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-sasl"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// ---------- Config ----------

type Config struct {
	Destination DestConfig     `json:"destination"`
	Sources     []SourceConfig `json:"sources"`
}

type DestConfig struct {
	Host        string `json:"host"`
	User        string `json:"user"`
	Pass        string `json:"pass"`
	Security    string `json:"security"`
	ReportLabel string `json:"report_label"` // folder for daily digest; empty = disabled
	SkipVerify  bool   `json:"skip_verify"`

	// Gmail OAuth2
	Provider        string `json:"provider"`
	CredentialsFile string `json:"credentials_file"`
	TokenFile       string `json:"token_file"`
}

type SourceConfig struct {
	Host          string      `json:"host"`
	User          string      `json:"user"`
	Pass          string      `json:"pass"`
	Security      string      `json:"security"`
	SkipVerify    bool        `json:"skip_verify"`
	RetentionDays int         `json:"retention_days"` // 0 = keep forever, >0 = prune after N days
	Mappings      []FolderMap `json:"mappings"`

	// Gmail OAuth2
	Provider        string `json:"provider"`
	CredentialsFile string `json:"credentials_file"`
	TokenFile       string `json:"token_file"`
}

type FolderMap struct {
	From   string   `json:"from"`
	To     []string `json:"to"`               // one or more destination folders; string or array
	Labels []string `json:"labels,omitempty"` // IMAP keywords set on append (server-dependent)
}

// UnmarshalJSON lets "to" be either a plain string or an array of strings
// so existing configs with "to": "INBOX" continue to work alongside
// the newer "to": ["INBOX", "Label"] form.
func (f *FolderMap) UnmarshalJSON(b []byte) error {
	// Use an alias to avoid infinite recursion.
	type Alias struct {
		From   string          `json:"from"`
		To     json.RawMessage `json:"to"`
		Labels []string        `json:"labels,omitempty"`
	}
	var a Alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	f.From = a.From
	f.Labels = a.Labels
	if len(a.To) > 0 {
		// Try array first, fall back to plain string.
		if err := json.Unmarshal(a.To, &f.To); err != nil {
			var s string
			if err2 := json.Unmarshal(a.To, &s); err2 != nil {
				return err2
			}
			f.To = []string{s}
		}
	}
	return nil
}

// ---------- Error log (daily digest) ----------

type errorEntry struct {
	ts    time.Time
	msg   string
	count int
}

type errorLog struct {
	mu      sync.Mutex
	entries []*errorEntry
	index   map[string]*errorEntry // keyed by msg for deduplication
}

// add records a message in the digest buffer and writes it to stdout.
// Identical repeated messages increment a counter rather than adding new lines.
func (e *errorLog) add(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Println(msg)
	e.mu.Lock()
	defer e.mu.Unlock()
	if existing, ok := e.index[msg]; ok {
		existing.count++
		existing.ts = time.Now()
		return
	}
	entry := &errorEntry{ts: time.Now(), msg: msg, count: 1}
	e.entries = append(e.entries, entry)
	e.index[msg] = entry
}

// drain returns all collected entries and resets the buffer.
func (e *errorLog) drain() []*errorEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.entries
	e.entries = nil
	e.index = make(map[string]*errorEntry)
	return out
}

var errLog = &errorLog{index: make(map[string]*errorEntry)}

// ---------- In-memory dedup cache ----------

type cacheEntry struct {
	ts time.Time
}

var (
	msgCache = make(map[string]cacheEntry)
	cacheMux sync.Mutex
	cacheTTL = 24 * time.Hour
)

func cacheSeen(id string) bool {
	cacheMux.Lock()
	defer cacheMux.Unlock()
	if e, ok := msgCache[id]; ok {
		if time.Since(e.ts) < cacheTTL {
			return true
		}
		delete(msgCache, id)
	}
	return false
}

func cacheAdd(id string) {
	cacheMux.Lock()
	msgCache[id] = cacheEntry{ts: time.Now()}
	cacheMux.Unlock()
}

// ---------- DB ----------

func initDB(path string) *sql.DB {
	db, err := sql.Open("sqlite3", path+"?_journal=WAL&_sync=1")
	if err != nil {
		log.Fatal("DB open failed:", err)
	}
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS folder_offsets (
		account_folder TEXT PRIMARY KEY,
		last_uid       INTEGER
	);
	CREATE TABLE IF NOT EXISTS sync_state (
		msgid      TEXT PRIMARY KEY,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`)
	if err != nil {
		log.Fatal("DB init failed:", err)
	}
	return db
}

// ---------- Backoff ----------

func backoff(attempt int) time.Duration {
	base := time.Second * time.Duration(1<<attempt)
	jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
	if base > time.Minute {
		base = time.Minute
	}
	return base + jitter
}

// ---------- OAuth2 ----------

func loadOAuthToken(credFile, tokenFile string) (*oauth2.Token, *oauth2.Config, error) {
	b, err := os.ReadFile(credFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read credentials %q: %w", credFile, err)
	}
	cfg, err := google.ConfigFromJSON(b, "https://mail.google.com/")
	if err != nil {
		return nil, nil, fmt.Errorf("parse credentials: %w", err)
	}
	if data, err := os.ReadFile(tokenFile); err == nil {
		var tok oauth2.Token
		if json.Unmarshal(data, &tok) == nil {
			return &tok, cfg, nil
		}
	}
	authURL := cfg.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("\nOpen this URL to authorise Gmail access:\n%s\n\nPaste the code: ", authURL)
	var code string
	if _, err := fmt.Scan(&code); err != nil {
		return nil, nil, fmt.Errorf("read auth code: %w", err)
	}
	tok, err := cfg.Exchange(context.Background(), code)
	if err != nil {
		return nil, nil, fmt.Errorf("token exchange: %w", err)
	}
	if data, err := json.Marshal(tok); err == nil {
		_ = os.WriteFile(tokenFile, data, 0600)
	}
	return tok, cfg, nil
}

func freshAccessToken(cfg *oauth2.Config, tok *oauth2.Token, tokenFile string) (string, *oauth2.Token, error) {
	if !tok.Valid() {
		newTok, err := cfg.TokenSource(context.Background(), tok).Token()
		if err != nil {
			return "", nil, fmt.Errorf("token refresh: %w", err)
		}
		if data, err := json.Marshal(newTok); err == nil {
			_ = os.WriteFile(tokenFile, data, 0600)
		}
		tok = newTok
	}
	return tok.AccessToken, tok, nil
}

// ---------- Connection ----------

func dial(host, security string, skipVerify bool) (*client.Client, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: skipVerify}
	switch strings.ToLower(security) {
	case "ssl":
		return client.DialTLS(host, tlsCfg)
	default:
		c, err := client.Dial(host)
		if err != nil {
			return nil, err
		}
		if strings.ToLower(security) == "tls" {
			if err := c.StartTLS(tlsCfg); err != nil {
				c.Logout()
				return nil, err
			}
		}
		return c, nil
	}
}

func connectAndLogin(
	host, security, user, pass, provider, credFile, tokenFile string,
	skipVerify bool,
) (*client.Client, *oauth2.Config, *oauth2.Token, error) {
	c, err := dial(host, security, skipVerify)
	if err != nil {
		return nil, nil, nil, err
	}
	if strings.ToLower(provider) == "gmail" {
		tok, cfg, err := loadOAuthToken(credFile, tokenFile)
		if err != nil {
			c.Logout()
			return nil, nil, nil, fmt.Errorf("gmail oauth load: %w", err)
		}
		accessToken, tok, err := freshAccessToken(cfg, tok, tokenFile)
		if err != nil {
			c.Logout()
			return nil, nil, nil, err
		}
		if err := c.Authenticate(sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Username: user,
			Token:    accessToken,
		})); err != nil {
			c.Logout()
			return nil, nil, nil, fmt.Errorf("gmail oauthbearer: %w", err)
		}
		return c, cfg, tok, nil
	}
	if err := c.Login(user, pass); err != nil {
		c.Logout()
		return nil, nil, nil, err
	}
	return c, nil, nil, nil
}

// ---------- Destination client ----------

const maxDestAttempts = 8

// DestClient is a single shared, thread-safe IMAP append connection with
// automatic reconnection. All source workers and the reporter use the same instance.
type DestClient struct {
	conf DestConfig

	mu       sync.Mutex
	c        *client.Client
	oauthCfg *oauth2.Config
	oauthTok *oauth2.Token
}

func (d *DestClient) redial() error {
	if d.c != nil {
		_ = d.c.Logout()
		d.c = nil
	}
	cfg := d.conf
	c, oCfg, oTok, err := connectAndLogin(
		cfg.Host, cfg.Security, cfg.User, cfg.Pass,
		cfg.Provider, cfg.CredentialsFile, cfg.TokenFile,
		cfg.SkipVerify,
	)
	if err != nil {
		return err
	}
	d.c, d.oauthCfg, d.oauthTok = c, oCfg, oTok
	return nil
}

func (d *DestClient) ensureConn() error {
	if d.c != nil {
		return nil
	}
	return d.redial()
}

// Append delivers r to label with flags, retrying with exponential backoff.
// flags should reflect the source message's read state (\Seen or empty).
// Pass an explicit empty slice []string{} to force delivery as unread.
func (d *DestClient) Append(ctx context.Context, label string, flags []string, r imap.Literal) error {
	_, err := d.appendGetUID(ctx, label, flags, r)
	return err
}

// appendGetUID appends r to label and returns the UID assigned by the server
// via the UIDPLUS APPENDUID response code (RFC 4315). Returns uid=0 if the
// server does not advertise UIDPLUS or omits the code — label STORE is skipped
// in that case. Uses c.Execute directly so the tagged OK StatusResp is
// accessible; no external uidplus package required.
func (d *DestClient) appendGetUID(ctx context.Context, label string, flags []string, r imap.Literal) (uid uint32, err error) {
	for attempt := 0; attempt < maxDestAttempts; attempt++ {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}

		d.mu.Lock()
		connErr := d.ensureConn()
		if connErr != nil {
			d.mu.Unlock()
			wait := backoff(attempt)
			errLog.add("dest connect error (attempt %d/%d): %v — retry in %s",
				attempt+1, maxDestAttempts, connErr, wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return 0, ctx.Err()
			}
			continue
		}

		appendErr := d.c.Append(label, flags, time.Now(), r)
		var appendUID uint32
		if appendErr == nil {
			// UIDPLUS servers return [APPENDUID validity uid] in the tagged OK.
			// go-imap v1 exposes this via the MailboxStatus after a successful
			// Append when the server sends APPENDUID. We read it from the
			// mailbox status UIDNEXT shortcut: select the mailbox and read back
			// the last assigned UID via STATUS UIDNEXT-1.
			// Simpler: parse directly from the StatusResp code.
			// go-imap's Append() swallows the StatusResp internally, but the
			// APPENDUID code is also delivered as a StatusUpdate on the Updates
			// channel when c.Updates is set.
			//
			// Since c.Updates is nil here (we don't set it on the dest client),
			// we use STATUS UIDNEXT as a reliable fallback: the just-appended
			// message UID == UIDNEXT - 1.
			if status, serr := d.c.Status(label, []imap.StatusItem{imap.StatusUidNext}); serr == nil && status.UidNext > 0 {
				appendUID = status.UidNext - 1
			}
		} else {
			_ = d.c.Logout()
			d.c = nil
		}
		d.mu.Unlock()

		if appendErr == nil {
			return appendUID, nil
		}

		wait := backoff(attempt)
		errLog.add("dest append error (attempt %d/%d): %v — retry in %s",
			attempt+1, maxDestAttempts, appendErr, wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	return 0, fmt.Errorf("dest: exhausted %d attempts appending to %q", maxDestAttempts, label)
}

// ---------- Gmail label worker ----------

// labelJob is a single X-GM-LABELS STORE request.
type labelJob struct {
	mailbox string
	uid     uint32
	labels  []string
	msgID   string // for logging only
}

// LabelWorker maintains a long-lived IMAP connection to the Gmail destination
// dedicated to serialized X-GM-LABELS STORE commands. It is only started when
// the destination provider is Gmail and at least one mapping has labels.
//
// Source workers enqueue jobs via Enqueue. On shutdown, Run drains the queue,
// applies any remaining STOREs, then logs out and returns.
type LabelWorker struct {
	conf DestConfig
	jobs chan labelJob
}

// newLabelWorker creates a LabelWorker with a buffered queue of capacity cap.
func newLabelWorker(conf DestConfig, cap int) *LabelWorker {
	return &LabelWorker{conf: conf, jobs: make(chan labelJob, cap)}
}

// Enqueue adds a job to the queue. Non-blocking: if the queue is full the job
// is dropped and an error is logged (avoids blocking source workers).
func (lw *LabelWorker) Enqueue(mailbox string, uid uint32, labels []string, msgID string) {
	select {
	case lw.jobs <- labelJob{mailbox: mailbox, uid: uid, labels: labels, msgID: msgID}:
	default:
		errLog.add("label queue full — X-GM-LABELS dropped for msg %s → %q", msgID, mailbox)
	}
}

// Run processes jobs until ctx is cancelled, then drains the remaining queue
// before returning. Keeps a single IMAP connection alive, reconnecting on
// failure with the same exponential-backoff strategy as DestClient.
func (lw *LabelWorker) Run(ctx context.Context) {
	log.Println("label worker: started")
	var c *client.Client

	connect := func() bool {
		for attempt := 0; attempt < maxDestAttempts; attempt++ {
			nc, _, _, err := connectAndLogin(
				lw.conf.Host, lw.conf.Security, lw.conf.User, lw.conf.Pass,
				lw.conf.Provider, lw.conf.CredentialsFile, lw.conf.TokenFile,
				lw.conf.SkipVerify,
			)
			if err == nil {
				c = nc
				return true
			}
			wait := backoff(attempt)
			errLog.add("label worker connect error (attempt %d/%d): %v — retry in %s",
				attempt+1, maxDestAttempts, err, wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				// During shutdown we still want to drain, so keep trying briefly.
			}
		}
		return false
	}

	store := func(job labelJob) {
		if job.uid == 0 {
			errLog.add("label worker: uid=0 for msg %s → %q, skipping", job.msgID, job.mailbox)
			return
		}
		for attempt := 0; attempt < maxDestAttempts; attempt++ {
			if c == nil && !connect() {
				errLog.add("label worker: could not reconnect for msg %s → %q", job.msgID, job.mailbox)
				return
			}
			if _, err := c.Select(job.mailbox, false); err != nil {
				errLog.add("label worker: select %q failed (attempt %d): %v", job.mailbox, attempt+1, err)
				_ = c.Logout()
				c = nil
				select {
				case <-time.After(backoff(attempt)):
				case <-ctx.Done():
				}
				continue
			}
			set := new(imap.SeqSet)
			set.AddNum(job.uid)
			labelList := make([]interface{}, len(job.labels))
			for i, l := range job.labels {
				labelList[i] = l
			}
			storeItem := imap.StoreItem(strings.Replace(
				string(imap.FormatFlagsOp(imap.AddFlags, true)), "FLAGS", "X-GM-LABELS", 1,
			))
			if err := c.UidStore(set, storeItem, labelList, nil); err != nil {
				errLog.add("label worker: X-GM-LABELS store failed for msg %s → %q (attempt %d): %v",
					job.msgID, job.mailbox, attempt+1, err)
				_ = c.Logout()
				c = nil
				select {
				case <-time.After(backoff(attempt)):
				case <-ctx.Done():
				}
				continue
			}
			return // success
		}
		errLog.add("label worker: exhausted retries for msg %s → %q", job.msgID, job.mailbox)
	}

	// Main loop: process jobs until ctx cancelled, then drain remainder.
	for {
		select {
		case job := <-lw.jobs:
			store(job)
		case <-ctx.Done():
			// Drain remaining jobs before exiting.
			remaining := len(lw.jobs)
			if remaining > 0 {
				log.Printf("label worker: draining %d queued label job(s) before shutdown", remaining)
			}
			for len(lw.jobs) > 0 {
				store(<-lw.jobs)
			}
			if c != nil {
				_ = c.Logout()
			}
			log.Println("label worker: stopped")
			return
		}
	}
}

// ---------- Retention pruner ----------

// pruneFolder marks messages older than retentionDays as \Deleted and expunges them.
// No-op when retentionDays is 0. Called on source folders only, never destination.
func pruneFolder(ctx context.Context, c *client.Client, user, folder string, retentionDays int) {
	if retentionDays <= 0 || ctx.Err() != nil {
		return
	}

	cutoff := time.Now().AddDate(0, 0, -retentionDays)

	criteria := imap.NewSearchCriteria()
	criteria.Before = cutoff

	uids, err := c.UidSearch(criteria)
	if err != nil {
		errLog.add("retention search failed on %s/%s: %v", user, folder, err)
		return
	}
	if len(uids) == 0 {
		return
	}

	set := new(imap.SeqSet)
	for _, uid := range uids {
		set.AddNum(uid)
	}

	storeItem := imap.FormatFlagsOp(imap.AddFlags, true)
	if err := c.UidStore(set, storeItem, []interface{}{imap.DeletedFlag}, nil); err != nil {
		errLog.add("retention flag failed on %s/%s: %v", user, folder, err)
		return
	}

	if err := c.Expunge(nil); err != nil {
		errLog.add("retention expunge failed on %s/%s: %v", user, folder, err)
		return
	}

	log.Printf("retention: pruned %d message(s) older than %d days from %s/%s",
		len(uids), retentionDays, user, folder)
}

// ---------- Sync ----------

// syncFolder fetches new messages from src and appends them to every folder
// listed in to, with optional IMAP keyword labels set on each append.
// When to has a single entry the body literal is streamed directly with no
// buffering. When to has multiple entries the body is read into memory once
// and replayed — unavoidable since an io.Reader can only be consumed once.
func syncFolder(ctx context.Context, c *client.Client, dest *DestClient, lw *LabelWorker, db *sql.DB, user, folder string, to, labels []string) {
	key := user + ":" + folder

	var lastUID uint32
	_ = db.QueryRow("SELECT last_uid FROM folder_offsets WHERE account_folder = ?", key).Scan(&lastUID)

	criteria := imap.NewSearchCriteria()
	if lastUID > 0 {
		set := new(imap.SeqSet)
		set.AddRange(lastUID+1, 0)
		criteria.Uid = set
	}

	uids, err := c.UidSearch(criteria)
	if err != nil {
		errLog.add("UID search error on %s/%s: %v", user, folder, err)
		return
	}
	log.Printf("DEBUG syncFolder %s/%s: lastUID=%d found=%d uids", user, folder, lastUID, len(uids))

	var maxUID uint32

	for _, uid := range uids {
		if ctx.Err() != nil {
			return
		}
		if uid > maxUID {
			maxUID = uid
		}

		set := new(imap.SeqSet)
		set.AddNum(uid)

		// Fetch envelope and flags in one round trip.
		envCh := make(chan *imap.Message, 1)
		go c.UidFetch(set, []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags}, envCh)
		msg := <-envCh

		if msg == nil || msg.Envelope == nil {
			continue
		}

		id := msg.Envelope.MessageId
		if id == "" {
			// No Message-ID — synthesise a stable dedup key from account+folder+uid.
			// This is not globally unique but is stable across restarts for this source.
			id = fmt.Sprintf("__noid__%s_%s_%d", user, folder, uid)
		}

		// Build destination flags: mirror \Seen, then append keywords.
		destFlags := []string{}
		for _, f := range msg.Flags {
			if f == imap.SeenFlag {
				destFlags = append(destFlags, imap.SeenFlag)
				break
			}
		}
		if len(labels) > 0 {
			destFlags = append(destFlags, labels...)
		}

		// Determine which destinations still need this message.
		pending := make([]string, 0, len(to))
		for _, dst := range to {
			dedupKey := id + "\x00" + dst
			if cacheSeen(dedupKey) {
				log.Printf("DEBUG uid=%d id=%q dst=%q — skipped (cache)", uid, id, dst)
				continue
			}
			var exists string
			_ = db.QueryRow("SELECT msgid FROM sync_state WHERE msgid = ?", dedupKey).Scan(&exists)
			if exists != "" {
				log.Printf("DEBUG uid=%d id=%q dst=%q — skipped (db)", uid, id, dst)
				cacheAdd(dedupKey)
				continue
			}
			pending = append(pending, dst)
		}
		if len(pending) == 0 {
			continue
		}

		// Fetch body.
		section := &imap.BodySectionName{}
		bodyCh := make(chan *imap.Message, 1)
		go c.UidFetch(set, []imap.FetchItem{section.FetchItem()}, bodyCh)
		body := <-bodyCh

		if body == nil {
			errLog.add("nil fetch response for uid %d in %s/%s", uid, user, folder)
			continue
		}
		literal := body.GetBody(section)
		if literal == nil {
			errLog.add("nil body literal for uid %d in %s/%s", uid, user, folder)
			continue
		}

		// Single destination — stream directly, no buffering.
		// Multiple destinations — buffer once, replay for each.
		var rawBody []byte
		if len(pending) > 1 {
			rawBody, err = io.ReadAll(literal)
			if err != nil {
				errLog.add("body read error for uid %d in %s/%s: %v", uid, user, folder, err)
				continue
			}
		}

		for _, dst := range pending {
			if ctx.Err() != nil {
				return
			}
			var appendUID uint32
			var appendErr error
			if rawBody != nil {
				appendUID, appendErr = dest.appendGetUID(ctx, dst, destFlags, bytes.NewReader(rawBody))
			} else {
				appendUID, appendErr = dest.appendGetUID(ctx, dst, destFlags, literal)
			}
			if appendErr != nil {
				errLog.add("append failed for msg %s → %q (%s/%s): %v", id, dst, user, folder, appendErr)
				continue
			}
			log.Printf("DEBUG uid=%d id=%q → %q appended OK (destUID=%d)", uid, id, dst, appendUID)
			// For Gmail destinations with labels, enqueue a X-GM-LABELS STORE
			// to the dedicated label worker.
			if lw != nil && len(labels) > 0 {
				lw.Enqueue(dst, appendUID, labels, id)
			}
			dedupKey := id + "\x00" + dst
			_, _ = db.Exec("INSERT INTO sync_state (msgid) VALUES (?)", dedupKey)
			cacheAdd(dedupKey)
		}
	}

	if maxUID > 0 {
		_, _ = db.Exec("INSERT OR REPLACE INTO folder_offsets VALUES (?, ?)", key, maxUID)
	}
}

// ---------- Source worker ----------

// monitorSource manages the full lifecycle for one source account.
// Connects, syncs all mappings, watches for new mail, reconnects on failure.
// No goto — uses a clean helper function for the connected phase.
func monitorSource(ctx context.Context, src SourceConfig, dest *DestClient, lw *LabelWorker, db *sql.DB) {
	attempt := 0

	for {
		if ctx.Err() != nil {
			return
		}

		c, _, _, err := connectAndLogin(
			src.Host, src.Security, src.User, src.Pass,
			src.Provider, src.CredentialsFile, src.TokenFile,
			src.SkipVerify,
		)
		if err != nil {
			wait := backoff(attempt)
			errLog.add("source connect failed (%s, attempt %d): %v — retry in %s",
				src.Host, attempt+1, err, wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
			attempt++
			continue
		}

		attempt = 0
		log.Printf("source connected: %s (%s)", src.Host, src.User)

		if cleanExit := runMappings(ctx, c, src, dest, lw, db); cleanExit {
			return
		}
		log.Printf("connection lost to %s — reconnecting", src.Host)
	}
}

// runMappings runs the sync+polling loop for all folder mappings on an open connection.
// Returns true if the exit was due to ctx cancellation (clean shutdown),
// false if the connection broke and a reconnect is needed.
//
// Each mapping is synced in turn, then the loop repeats. Between full passes
// we IDLE on the first mapping's folder to wake immediately on new mail;
// all folders are re-synced on every wakeup regardless of which triggered it.
func runMappings(ctx context.Context, c *client.Client, src SourceConfig, dest *DestClient, lw *LabelWorker, db *sql.DB) (cleanExit bool) {
	defer c.Logout()

	for {
		// Sync every mapping in sequence.
		for _, m := range src.Mappings {
			if ctx.Err() != nil {
				return true
			}

			// Select read-write so pruneFolder can expunge if needed.
			if _, err := c.Select(m.From, false); err != nil {
				errLog.add("select %q failed on %s: %v — reconnecting", m.From, src.Host, err)
				return false
			}

			syncFolder(ctx, c, dest, lw, db, src.User, m.From, m.To, m.Labels)

			if src.RetentionDays > 0 {
				pruneFolder(ctx, c, src.User, m.From, src.RetentionDays)
			}
		}

		if ctx.Err() != nil {
			return true
		}

		// After syncing all folders, IDLE on the first mapping's folder to
		// receive push notifications for new mail. Fall back to a 10-minute
		// poll if the server doesn't support IDLE.
		if len(src.Mappings) > 0 {
			if _, err := c.Select(src.Mappings[0].From, false); err != nil {
				errLog.add("select %q for idle failed on %s: %v — reconnecting", src.Mappings[0].From, src.Host, err)
				return false
			}
		}

		updates := make(chan client.Update, 16)
		c.Updates = updates

		idleDone := make(chan struct{})
		idleErr := make(chan error, 1)
		go func() {
			idleErr <- c.Idle(idleDone, nil)
		}()

		select {
		case <-updates:
			// Server pushed an EXISTS or RECENT — new mail arrived.
			// Signal IDLE to stop, wait for the goroutine to finish.
			close(idleDone)
			if err := <-idleErr; err != nil {
				errLog.add("idle error on %s: %v — reconnecting", src.Host, err)
				c.Updates = nil
				return false
			}

		case err := <-idleErr:
			// IDLE returned on its own (server closed it, or not supported).
			if err != nil {
				errLog.add("idle ended on %s: %v — reconnecting", src.Host, err)
				c.Updates = nil
				return false
			}

		case <-time.After(10 * time.Minute):
			// Keepalive: exit IDLE, send NOOP, then re-sync.
			close(idleDone)
			<-idleErr
			if err := c.Noop(); err != nil {
				errLog.add("noop failed on %s: %v — reconnecting", src.Host, err)
				c.Updates = nil
				return false
			}

		case <-ctx.Done():
			close(idleDone)
			<-idleErr
			c.Updates = nil
			return true
		}

		c.Updates = nil
	}
}

// ---------- Report ----------

// buildReportEmail constructs a minimal plain-text RFC 5322 digest.
// Always delivered unread regardless of any config setting.
func buildReportEmail(destUser string, entries []*errorEntry) imap.Literal {
	now := time.Now()
	var b strings.Builder

	b.WriteString("From: IMAP Bridge <bridge@localhost>\r\n")
	b.WriteString(fmt.Sprintf("To: %s\r\n", destUser))
	b.WriteString(fmt.Sprintf("Subject: [imap-bridge] Daily report — %s\r\n", now.Format("2006-01-02")))
	b.WriteString(fmt.Sprintf("Date: %s\r\n", now.Format(time.RFC1123Z)))
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("\r\n")

	if len(entries) == 0 {
		b.WriteString("No errors or warnings in the past 24 hours.\r\n")
		b.WriteString("All sources are syncing cleanly.\r\n")
	} else {
		b.WriteString(fmt.Sprintf("%d distinct issue(s) in the past 24 hours:\r\n\r\n", len(entries)))
		for _, e := range entries {
			if e.count == 1 {
				b.WriteString(fmt.Sprintf("  [%s]  %s\r\n", e.ts.Format("15:04:05"), e.msg))
			} else {
				b.WriteString(fmt.Sprintf("  [%s]  %s  (x%d)\r\n", e.ts.Format("15:04:05"), e.msg, e.count))
			}
		}
		b.WriteString("\r\nFull details: docker logs imap-bridge\r\n")
	}

	return bytes.NewReader([]byte(b.String()))
}

func nextReportTime() time.Time {
	now := time.Now()
	t := time.Date(now.Year(), now.Month(), now.Day(), 7, 0, 0, 0, now.Location())
	if now.After(t) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

// runReporter fires at 07:00 local time each day, drains the error buffer,
// and appends a digest email to report_label. Disabled if report_label is empty.
func runReporter(ctx context.Context, dest *DestClient, destConf DestConfig) {
	if destConf.ReportLabel == "" {
		log.Println("report_label not configured — daily report disabled")
		return
	}

	log.Printf("daily report enabled → %q at 07:00 local", destConf.ReportLabel)

	for {
		next := nextReportTime()
		log.Printf("next report scheduled: %s", next.Format("2006-01-02 15:04:05"))

		select {
		case <-time.After(time.Until(next)):
		case <-ctx.Done():
			return
		}

		entries := errLog.drain()
		msg := buildReportEmail(destConf.User, entries)

		// Explicit empty flags — report always lands unread to catch your eye.
		if err := dest.Append(ctx, destConf.ReportLabel, []string{}, msg); err != nil {
			log.Printf("ERROR: failed to deliver daily report: %v", err)
		} else {
			log.Printf("daily report delivered to %q (%d distinct entries)", destConf.ReportLabel, len(entries))
		}
	}
}

// ---------- Folder discovery ----------

// logFolderStructure connects to an IMAP server, lists all folders, and logs
// them in a tree-like format. Called at startup when a source has no mappings
// defined, so the user can identify the correct folder names via docker logs.
func logFolderStructure(label, host, security, user, pass, provider, credFile, tokenFile string, skipVerify bool) {
	c, _, _, err := connectAndLogin(host, security, user, pass, provider, credFile, tokenFile, skipVerify)
	if err != nil {
		log.Printf("folder-discovery: could not connect to %s (%s): %v", label, host, err)
		return
	}
	defer c.Logout()

	mailboxes := make(chan *imap.MailboxInfo, 32)
	done := make(chan error, 1)
	go func() { done <- c.List("", "*", mailboxes) }()

	var folders []string
	for mb := range mailboxes {
		folders = append(folders, mb.Name)
	}
	if err := <-done; err != nil {
		log.Printf("folder-discovery: LIST failed for %s (%s): %v", label, host, err)
		return
	}

	log.Printf("folder-discovery: %s (%s) — %d folder(s):", label, user, len(folders))
	for _, name := range folders {
		log.Printf("  folder-discovery:   %s", name)
	}
}

// ---------- Main ----------

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	f, err := os.Open("config.json")
	if err != nil {
		log.Fatal("config load failed:", err)
	}
	var conf Config
	if err := json.NewDecoder(f).Decode(&conf); err != nil {
		log.Fatal("config parse failed:", err)
	}
	f.Close()

	// Folder discovery: if any source has no mappings, log both that source's
	// folder list and the destination's folder list, then exit. This lets the
	// user see exact folder names in docker logs before writing their config.
	needsDiscovery := false
	for _, src := range conf.Sources {
		if len(src.Mappings) == 0 {
			needsDiscovery = true
			break
		}
	}
	if needsDiscovery {
		log.Println("folder-discovery: one or more sources have no mappings — listing folders and exiting")
		d := conf.Destination
		logFolderStructure("destination", d.Host, d.Security, d.User, d.Pass, d.Provider, d.CredentialsFile, d.TokenFile, d.SkipVerify)
		for i, src := range conf.Sources {
			if len(src.Mappings) == 0 {
				logFolderStructure(fmt.Sprintf("source[%d]", i), src.Host, src.Security, src.User, src.Pass, src.Provider, src.CredentialsFile, src.TokenFile, src.SkipVerify)
			}
		}
		log.Println("folder-discovery: add mappings to config.json and restart")
		return
	}

	if err := os.MkdirAll("./data", 0755); err != nil {
		log.Fatal("mkdir data:", err)
	}

	db := initDB("./data/state.db")
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Single shared DestClient — all goroutines use the same underlying connection.
	dest := &DestClient{conf: conf.Destination}

	var wg sync.WaitGroup

	// Start a dedicated label worker for Gmail destinations. nil if not needed.
	var lw *LabelWorker
	if strings.ToLower(conf.Destination.Provider) == "gmail" {
		lw = newLabelWorker(conf.Destination, 256)
		wg.Add(1)
		go func() {
			defer wg.Done()
			lw.Run(ctx)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		runReporter(ctx, dest, conf.Destination)
	}()

	for _, src := range conf.Sources {
		wg.Add(1)
		go func(s SourceConfig) {
			defer wg.Done()
			monitorSource(ctx, s, dest, lw, db)
		}(src)
	}

	<-ctx.Done()
	log.Println("shutdown signal received")
	wg.Wait()
	log.Println("clean exit")
}
