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
	mrand "math/rand"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-sasl"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// ---------- Debug ----------

var debugEnabled bool

func debugLog(format string, args ...any) {
	if debugEnabled {
		log.Printf("DEBUG "+format, args...)
	}
}

// ---------- Config ----------

type Config struct {
	Destination DestConfig     `json:"destination"`
	Sources     []SourceConfig `json:"sources"`
	Debug       bool           `json:"debug"` // enable verbose per-message logging
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
	Host                 string      `json:"host"`
	User                 string      `json:"user"`
	Pass                 string      `json:"pass"`
	Security             string      `json:"security"`
	SkipVerify           bool        `json:"skip_verify"`
	RetentionDays        int         `json:"retention_days"`         // 0 = keep forever, >0 = prune after N days
	DisableIdle          bool        `json:"disable_idle"`           // fall back to polling; use when server IDLE is broken
	PollInterval         int         `json:"poll_interval"`          // poll interval in seconds when idle disabled or falls back; default 600
	SyncNewOnly          bool        `json:"sync_new_only"`          // on first run, skip existing mail and only sync new messages
	MaxErrorRetentionDays int        `json:"max_error_retention_days"` // stop retrying skipped messages after N days; 0 = retry forever
	Mappings             []FolderMap `json:"mappings"`

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

// cacheEvict removes all entries older than cacheTTL. Run once a day.
func cacheEvict() {
	cacheMux.Lock()
	defer cacheMux.Unlock()
	for id, e := range msgCache {
		if time.Since(e.ts) >= cacheTTL {
			delete(msgCache, id)
		}
	}
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
	);
	CREATE TABLE IF NOT EXISTS sync_failures (
		key        TEXT PRIMARY KEY,  -- account:folder:uid
		msgid      TEXT,
		failures   INTEGER DEFAULT 0,
		last_error TEXT,
		skipped_at DATETIME
	);`)
	if err != nil {
		log.Fatal("DB init failed:", err)
	}
	return db
}

// ---------- Backoff ----------

func backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 10 {
		attempt = 10
	}
	base := time.Second * time.Duration(1<<attempt)
	jitter := time.Duration(mrand.Intn(1000)) * time.Millisecond
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
	fmt.Printf("\nOpen this URL to authorise Gmail access:\n%s\n\nPaste the code or full redirect URL: ", authURL)
	var input string
	if _, err := fmt.Scan(&input); err != nil {
		return nil, nil, fmt.Errorf("read auth code: %w", err)
	}
	// Accept either a bare code or the full redirect URL that Google sends back
	// (e.g. http://localhost/?code=4/0AX...&scope=...). Parse out the code param.
	code := input
	if m := regexp.MustCompile(`[?&]code=([^&]+)`).FindStringSubmatch(input); m != nil {
		code = m[1]
	}
	tok, err := cfg.Exchange(context.Background(), code)
	if err != nil {
		return nil, nil, fmt.Errorf("token exchange: %w", err)
	}
	if data, err := json.Marshal(tok); err == nil {
		_ = os.WriteFile(tokenFile, data, 0600)
	}
	log.Printf("OAuth2 authorisation successful — token saved to %s", tokenFile)
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

// maxMsgAttempts is the number of consecutive sync failures before a message
// is permanently skipped. Prevents a single poison message from blocking all
// subsequent mail in the same folder.
const maxMsgAttempts = 5

// DestClient is a single shared, thread-safe IMAP append connection with
// automatic reconnection. All source workers and the reporter use the same instance.
type DestClient struct {
	conf DestConfig

	mu         sync.Mutex
	c          *client.Client
	oauthCfg   *oauth2.Config
	oauthTok   *oauth2.Token
	hasUIDPLUS bool        // cached per connection; set during redial
	lastNoopAt time.Time   // guards keepalive rate across all source goroutines
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
	log.Printf("destination connected: %s (%s)", cfg.Host, cfg.User)
	// Detect UIDPLUS once per connection. Failure to query is treated as absent.
	if ok, err := c.Support("UIDPLUS"); err == nil {
		d.hasUIDPLUS = ok
	} else {
		d.hasUIDPLUS = false
	}
	if !d.hasUIDPLUS && strings.ToLower(cfg.Provider) == "gmail" {
		log.Printf("WARNING: destination %s does not advertise UIDPLUS — X-GM-LABELS will not be applied", cfg.Host)
	}
	return nil
}

func (d *DestClient) ensureConn() error {
	if d.c != nil {
		return nil
	}
	return d.redial()
}

// errDestDown is returned by appendGetUID when the destination connection
// cannot execute IMAP commands reliably (EOF, broken pipe, auth failure,
// or repeated failed reconnects). Distinct from a server-level message
// rejection so syncFolder can abort the pass without burning failure counts.
var errDestDown = fmt.Errorf("destination unreachable")

// Append delivers r to label with flags, retrying with exponential backoff.
func (d *DestClient) Append(ctx context.Context, label string, flags []string, r imap.Literal) error {
	_, err := d.appendGetUID(ctx, label, flags, r)
	return err
}

// appendGetUID appends r to label and returns the server-assigned UID.
//
// UID resolution is UIDPLUS-only (RFC 4315). If the destination supports
// UIDPLUS, the APPENDUID response code in the tagged OK is the sole UID
// source — exact and race-free. If UIDPLUS is absent, uid=0 is returned
// and the append is still considered successful; label STORE is skipped.
//
// The literal r is streamed directly — no buffering occurs here.
// Each call must receive its own independent reader; readers are never reused.
//
// Returns errDestDown only on connection-level failures (EOF, broken pipe,
// auth failure, repeated reconnect failures) — not on server NO/BAD responses.
func (d *DestClient) appendGetUID(ctx context.Context, label string, flags []string, r imap.Literal) (uid uint32, err error) {
	connFails := 0
	for attempt := 0; attempt < maxDestAttempts; attempt++ {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}

		d.mu.Lock()
		connErr := d.ensureConn()
		if connErr != nil {
			d.mu.Unlock()
			connFails++
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

		cmd := &commands.Append{
			Mailbox: label,
			Flags:   flags,
			Date:	time.Now(),
			Message: r,
		}
		status, execErr := d.c.Execute(cmd, nil)

		if execErr != nil {
			// Execute only returns non-nil err on network-level failures.
			// These are always connection faults — mark conn dead and count.
			_ = d.c.Logout()
			d.c = nil
			d.hasUIDPLUS = false
			connFails++
			d.mu.Unlock()
			wait := backoff(attempt)
			errLog.add("dest append error (attempt %d/%d): %v — retry in %s",
				attempt+1, maxDestAttempts, execErr, wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return 0, ctx.Err()
			}
			continue
		}

		// Execute returned nil err. Check status for server-level NO/BAD.
		// These are message-level rejections — connection stays alive.
		if status != nil && (status.Type == imap.StatusRespNo || status.Type == imap.StatusRespBad) {
			d.mu.Unlock()
			wait := backoff(attempt)
			errLog.add("dest append rejected (attempt %d/%d): %s %s — retry in %s",
				attempt+1, maxDestAttempts, status.Type, status.Info, wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return 0, ctx.Err()
			}
			continue
		}

		// Append succeeded. Extract APPENDUID (Gmail sometimes returns it as string).
		appendUID := uint32(0)
		if status != nil && strings.EqualFold(string(status.Code), "APPENDUID") && len(status.Arguments) >= 2 {
			raw := status.Arguments[1]
			switch v := raw.(type) {
			case uint32:
				appendUID = v
				debugLog("appendGetUID: UIDPLUS success → APPENDUID = %d (uint32)", appendUID)
			case string:
				if u, err := strconv.ParseUint(v, 10, 32); err == nil {
					appendUID = uint32(u)
					debugLog("appendGetUID: UIDPLUS success → APPENDUID = %d (parsed from string %q)", appendUID, v)
				} else {
					debugLog("appendGetUID: APPENDUID string parse failed: %v (value=%q)", err, v)
				}
			default:
				debugLog("appendGetUID: APPENDUID code present but arg[1] is unexpected type (type=%T, value=%v)", raw, raw)
			}
		} else if status != nil {
			debugLog("appendGetUID: no APPENDUID code received (code=%q, args=%d) — uid=0 (labels skipped if any)",
				status.Code, len(status.Arguments))
		} else {
			debugLog("appendGetUID: status was nil — uid=0 (labels skipped if any)")
		}

		d.mu.Unlock()
		return appendUID, nil
	}

	if connFails == maxDestAttempts {
		return 0, errDestDown
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

	mu sync.Mutex
	c  *client.Client // guarded by mu; nil when not connected
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

// noop sends a NOOP on the label worker connection if it is currently open.
// Called from the source sync loop to keep the connection alive between jobs.
func (lw *LabelWorker) noop() {
	lw.mu.Lock()
	c := lw.c
	lw.mu.Unlock()
	if c == nil {
		return
	}
	if err := c.Noop(); err != nil {
		debugLog("label worker keepalive noop failed: %v — will reconnect on next job", err)
		lw.mu.Lock()
		lw.c = nil
		lw.mu.Unlock()
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
				lw.mu.Lock()
				lw.c = nc
				lw.mu.Unlock()
				log.Printf("label worker connected: %s (%s)", lw.conf.Host, lw.conf.User)
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
				lw.mu.Lock()
				lw.c = nil
				lw.mu.Unlock()
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
				lw.mu.Lock()
				lw.c = nil
				lw.mu.Unlock()
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
				lw.mu.Lock()
				lw.c = nil
				lw.mu.Unlock()
			}
			log.Println("label worker: stopped")
			return
		}
	}
}

// ---------- Retention pruner ----------

// pruneFolder marks messages older than retentionDays as \Deleted and expunges them.
// No-op when retentionDays <= 0. retentionDays=-1 means sync all history, never prune.
// Called on source folders only, never destination.
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
func syncFolder(ctx context.Context, c *client.Client, dest *DestClient, lw *LabelWorker, db *sql.DB, user, folder string, to, labels []string, syncNewOnly bool, retentionDays int) (allDelivered bool) {
	key := user + ":" + folder

	var lastUID uint32
	_ = db.QueryRow("SELECT last_uid FROM folder_offsets WHERE account_folder = ?", key).Scan(&lastUID)

	// First run with sync_new_only: skip all existing mail by initialising the
	// offset to the current highest UID. Only applies when retention_days <= 0 —
	// with positive retention you want historical mail within the window.
	if lastUID == 0 && syncNewOnly && retentionDays <= 0 {
		if status, err := c.Status(folder, []imap.StatusItem{imap.StatusUidNext}); err == nil && status.UidNext > 1 {
			lastUID = status.UidNext - 1
			_, _ = db.Exec("INSERT OR REPLACE INTO folder_offsets VALUES (?, ?)", key, lastUID)
			log.Printf("sync_new_only: %s/%s initialised at UID %d — existing mail skipped", user, folder, lastUID)
		}
	}

	criteria := imap.NewSearchCriteria()
	if lastUID > 0 {
		set := new(imap.SeqSet)
		set.AddRange(lastUID+1, 0)
		criteria.Uid = set
	}

	uids, err := c.UidSearch(criteria)
	if err != nil {
		errLog.add("UID search error on %s/%s: %v", user, folder, err)
		return false
	}
	debugLog("syncFolder %s/%s: lastUID=%d found=%d uids", user, folder, lastUID, len(uids))

	allDelivered = true
	var maxUID uint32

	for _, uid := range uids {
		if ctx.Err() != nil {
			return
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
		// Use case-insensitive comparison — some servers return \seen lowercase.
		destFlags := []string{}
		for _, f := range msg.Flags {
			if strings.EqualFold(f, imap.SeenFlag) {
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
				debugLog("uid=%d id=%q dst=%q — skipped (cache)", uid, id, dst)
				continue
			}
			var exists string
			_ = db.QueryRow("SELECT msgid FROM sync_state WHERE msgid = ?", dedupKey).Scan(&exists)
			if exists != "" {
				debugLog("uid=%d id=%q dst=%q — skipped (db)", uid, id, dst)
				cacheAdd(dedupKey)
				continue
			}
			pending = append(pending, dst)
		}
		if len(pending) == 0 {
			continue
		}

		// Check failure record. If skipped_at is set, still in holdoff — advance past it.
		// If the record exists but skipped_at is NULL, it's queued for retry today.
		// In both cases, first verify the UID still exists on source — if the user
		// deleted the message the failure clears itself automatically.
		failKey := fmt.Sprintf("%s:%s:%d", user, folder, uid)
		var failureCount int
		var skippedAt sql.NullString
		_ = db.QueryRow("SELECT failures, skipped_at FROM sync_failures WHERE key = ?", failKey).Scan(&failureCount, &skippedAt)
		if failureCount > 0 {
			// Verify message still exists on source.
			existsSet := new(imap.SeqSet)
			existsSet.AddNum(uid)
			existsCh := make(chan *imap.Message, 1)
			go c.UidFetch(existsSet, []imap.FetchItem{imap.FetchUid}, existsCh)
			existsMsg := <-existsCh
			if existsMsg == nil {
				// Message gone from source — clear failure record and advance offset.
				_, _ = db.Exec("DELETE FROM sync_failures WHERE key = ?", failKey)
				if uid > maxUID {
					maxUID = uid
				}
				debugLog("uid=%d auto-cleared from failures — no longer on source", uid)
				continue
			}
			if skippedAt.Valid {
				// Still in holdoff — advance past it without retrying.
				if uid > maxUID {
					maxUID = uid
				}
				continue
			}
			// skipped_at is NULL — daily retry has reset it, fall through to attempt delivery.
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

		// Body handling: stream directly to a single destination; buffer once
		// only when multiple destinations require independent readers.
		// io.Reader can only be consumed once — buffering is unavoidable for
		// multi-destination but must not occur for the single-destination path.
		var rawBody []byte
		if len(pending) > 1 {
			var readErr error
			rawBody, readErr = io.ReadAll(literal)
			if readErr != nil {
				errLog.add("body read error for uid %d in %s/%s: %v", uid, user, folder, readErr)
				continue
			}
		}

		msgDelivered := true
		var lastAppendErr string
		for _, dst := range pending {
			if ctx.Err() != nil {
				return
			}
			var r imap.Literal
			if rawBody != nil {
				r = bytes.NewReader(rawBody)
			} else {
				r = literal
			}
			appendUID, appendErr := dest.appendGetUID(ctx, dst, destFlags, r)
			if appendErr != nil {
				if appendErr == errDestDown {
					// Destination is unreachable — abort this entire sync pass.
					// Don't increment failure counters; this is an infrastructure
					// problem not a message problem.
					errLog.add("sync aborted for %s/%s — destination unreachable", user, folder)
					allDelivered = false
					return
				}
				errLog.add("append failed for msg %s → %q (%s/%s): %v", id, dst, user, folder, appendErr)
				msgDelivered = false
				allDelivered = false
				lastAppendErr = appendErr.Error()
				continue
			}
			debugLog("uid=%d id=%q → %q appended OK (destUID=%d)", uid, id, dst, appendUID)
			// Labels via X-GM-LABELS require a UIDPLUS-derived UID.
			// If uid=0 (UIDPLUS absent), skip label STORE silently —
			// a startup warning is emitted if this condition is expected.
			if lw != nil && len(labels) > 0 && appendUID > 0 {
				lw.Enqueue(dst, appendUID, labels, id)
			}
			dedupKey := id + "\x00" + dst
			_, _ = db.Exec("INSERT INTO sync_state (msgid) VALUES (?)", dedupKey)
			cacheAdd(dedupKey)
		}
		// Only advance the UID offset if every destination for this message
		// was delivered. If any failed, increment the failure counter.
		// After maxMsgAttempts, permanently skip the message so it doesn't
		// block all subsequent mail.
		if msgDelivered && uid > maxUID {
			maxUID = uid
			// Clear any prior failure record on success.
			_, _ = db.Exec("DELETE FROM sync_failures WHERE key = ?", failKey)
		} else if !msgDelivered {
			var failures int
			_ = db.QueryRow("SELECT failures FROM sync_failures WHERE key = ?", failKey).Scan(&failures)
			failures++
			if failures >= maxMsgAttempts {
				_, _ = db.Exec(`INSERT OR REPLACE INTO sync_failures (key, msgid, failures, last_error, skipped_at)
					VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)`, failKey, id, failures, lastAppendErr)
				errLog.add("SKIPPED uid=%d (%s/%s) after %d failed attempts — message will not be retried; last error: %s",
					uid, user, folder, failures, lastAppendErr)
				if uid > maxUID {
					maxUID = uid
				}
			} else {
				_, _ = db.Exec(`INSERT OR REPLACE INTO sync_failures (key, msgid, failures, last_error)
					VALUES (?, ?, ?, ?)`, failKey, id, failures, lastAppendErr)
			}
		}
	}

	if maxUID > 0 {
		_, _ = db.Exec("INSERT OR REPLACE INTO folder_offsets VALUES (?, ?)", key, maxUID)
	}
	return allDelivered
}

// ---------- Source worker ----------

// monitorSource manages the full lifecycle for one source account.
// Connects, syncs all mappings, watches for new mail, reconnects on failure.
// No goto — uses a clean helper function for the connected phase.
func monitorSource(ctx context.Context, src SourceConfig, dest *DestClient, lw *LabelWorker, db *sql.DB) {
	attempt := 0 // connect-failure backoff counter
	reconnect := 0 // counts rapid reconnects (connection established but lost quickly)

	for {
		if ctx.Err() != nil {
			return
		}

		// If we're reconnecting after a short-lived connection, apply backoff
		// to avoid spinning when the server accepts TCP but fails on commands.
		if reconnect > 0 {
			wait := backoff(reconnect)
			errLog.add("source reconnecting %s (attempt %d) — retry in %s", src.Host, reconnect, wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
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
			reconnect = 0
			continue
		}

		attempt = 0
		log.Printf("source connected: %s (%s)", src.Host, src.User)

		if cleanExit := runMappings(ctx, c, src, dest, lw, db); cleanExit {
			return
		}
		log.Printf("connection lost to %s — reconnecting", src.Host)
		reconnect++
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

	const keepaliveInterval = 3 * time.Minute

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

			syncOK := syncFolder(ctx, c, dest, lw, db, src.User, m.From, m.To, m.Labels, src.SyncNewOnly, src.RetentionDays)

			// Only prune if all messages were delivered successfully.
			// If any append failed, skip pruning to avoid deleting undelivered mail.
			if syncOK && src.RetentionDays > 0 {
				pruneFolder(ctx, c, src.User, m.From, src.RetentionDays)
			}
		}

		if ctx.Err() != nil {
			return true
		}

		// Keepalive: send NOOP on destination and label worker if already open,
		// but no more than once every 3 minutes globally across all sources.
		// lastNoopAt is on DestClient so the rate limit is shared.
		dest.mu.Lock()
		if time.Since(dest.lastNoopAt) >= keepaliveInterval {
			if dest.c != nil {
				if err := dest.c.Noop(); err != nil {
					debugLog("dest keepalive noop failed: %v — connection will redial on next append", err)
					_ = dest.c.Logout()
					dest.c = nil
				}
			}
			dest.lastNoopAt = time.Now()
			dest.mu.Unlock()
			if lw != nil {
				lw.noop()
			}
		} else {
			dest.mu.Unlock()
		}

		// Determine poll interval — default 600s (10 min).
		pollInterval := time.Duration(src.PollInterval) * time.Second
		if pollInterval <= 0 {
			pollInterval = 10 * time.Minute
		}

		if src.DisableIdle {
			// Pure polling mode — just wait the poll interval then re-sync.
			select {
			case <-time.After(pollInterval):
			case <-ctx.Done():
				return true
			}
			continue
		}

		// IDLE mode — select first folder and wait for push notification.
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
			close(idleDone)
			if err := <-idleErr; err != nil {
				errLog.add("idle error on %s: %v — reconnecting", src.Host, err)
				c.Updates = nil
				return false
			}

		case err := <-idleErr:
			// IDLE returned on its own (server closed it or doesn't support it).
			if err != nil {
				errLog.add("idle ended on %s: %v — reconnecting", src.Host, err)
				c.Updates = nil
				return false
			}
			// Brief holdoff to avoid spinning when IDLE is broken.
			holdoff := 20 * time.Second
			if pollInterval < holdoff {
				holdoff = pollInterval
			}
			select {
			case <-time.After(holdoff):
			case <-ctx.Done():
				c.Updates = nil
				return true
			}

		case <-time.After(pollInterval):
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
func buildReportEmail(destUser string, entries []*errorEntry, db *sql.DB) imap.Literal {
	now := time.Now()
	var b strings.Builder

	b.WriteString("From: IMAP Bridge <bridge@localhost>\r\n")
	b.WriteString(fmt.Sprintf("To: %s\r\n", destUser))
	b.WriteString(fmt.Sprintf("Subject: [imap-bridge] Daily report — %s\r\n", now.Format("2006-01-02")))
	b.WriteString(fmt.Sprintf("Date: %s\r\n", now.Format(time.RFC1123Z)))
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("\r\n")

	// Always show permanently skipped messages so they stay visible every day.
	type skippedRow struct {
		key       string
		msgid     string
		failures  int
		lastError string
		skippedAt string
	}
	var skipped []skippedRow
	if db != nil {
		rows, err := db.Query(`SELECT key, msgid, failures, last_error, skipped_at
			FROM sync_failures WHERE skipped_at IS NOT NULL ORDER BY skipped_at`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var r skippedRow
				_ = rows.Scan(&r.key, &r.msgid, &r.failures, &r.lastError, &r.skippedAt)
				skipped = append(skipped, r)
			}
		}
	}

	if len(skipped) > 0 {
		b.WriteString(fmt.Sprintf("⚠️  %d permanently skipped message(s) — action required:\r\n\r\n", len(skipped)))
		for _, r := range skipped {
			b.WriteString(fmt.Sprintf("  key:       %s\r\n", r.key))
			b.WriteString(fmt.Sprintf("  msg-id:    %s\r\n", r.msgid))
			b.WriteString(fmt.Sprintf("  skipped:   %s\r\n", r.skippedAt))
			b.WriteString(fmt.Sprintf("  reason:    %s\r\n\r\n", r.lastError))
		}
		b.WriteString("To clear and retry all skipped messages:\r\n")
		b.WriteString("  sqlite3 data/state.db \"DELETE FROM sync_failures;\"\r\n")
		b.WriteString("To clear a specific message:\r\n")
		b.WriteString("  sqlite3 data/state.db \"DELETE FROM sync_failures WHERE key='account:folder:uid';\"\r\n\r\n")
	}

	if len(entries) == 0 {
		if len(skipped) == 0 {
			b.WriteString("No errors or warnings in the past 24 hours.\r\n")
			b.WriteString("All sources are syncing cleanly.\r\n")
		}
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

// resetDailyRetries clears skipped_at on all sync_failures records so they
// are retried on the next sync pass. Records older than maxDays (per source
// config) are deleted entirely and their folder offset is advanced past them
// so they are not found again.
func resetDailyRetries(db *sql.DB, sources []SourceConfig) {
	// Expire records that have exceeded their source's max_error_retention_days.
	for _, src := range sources {
		if src.MaxErrorRetentionDays <= 0 {
			continue
		}
		prefix := src.User + ":"
		// Find expired records so we can advance the folder offset past them.
		rows, err := db.Query(`SELECT key FROM sync_failures
			WHERE key LIKE ? AND skipped_at IS NOT NULL
			AND julianday('now') - julianday(skipped_at) > ?`,
			prefix+"%", src.MaxErrorRetentionDays)
		if err == nil {
			for rows.Next() {
				var key string
				if rows.Scan(&key) != nil {
					continue
				}
				// key format: user:folder:uid — parse out folder and uid
				// and advance folder_offsets if this uid is beyond current offset.
				parts := strings.SplitN(key, ":", 3) // [user, folder, uid]
				if len(parts) == 3 {
					var uid uint32
					if _, err := fmt.Sscanf(parts[2], "%d", &uid); err == nil {
						folderKey := parts[0] + ":" + parts[1]
						var lastUID uint32
						_ = db.QueryRow("SELECT last_uid FROM folder_offsets WHERE account_folder = ?", folderKey).Scan(&lastUID)
						if uid > lastUID {
							_, _ = db.Exec("INSERT OR REPLACE INTO folder_offsets VALUES (?, ?)", folderKey, uid)
						}
					}
				}
			}
			rows.Close()
		}
		_, _ = db.Exec(`DELETE FROM sync_failures
			WHERE key LIKE ? AND skipped_at IS NOT NULL
			AND julianday('now') - julianday(skipped_at) > ?`,
			prefix+"%", src.MaxErrorRetentionDays)
	}
	// Reset skipped_at on all remaining records so they are retried today.
	_, _ = db.Exec(`UPDATE sync_failures SET skipped_at = NULL WHERE skipped_at IS NOT NULL`)

	// Prune sync_state rows by age. The dedup key format is "message-id\x00dest-folder"
	// with no source prefix, so pruning must be global. Use the longest retention
	// window across all sources — max(retention_days, max_error_retention_days) + 1,
	// minimum 7 days — so no source loses dedup coverage prematurely.
	const defaultSyncStateRetention = 7
	window := defaultSyncStateRetention
	for _, src := range sources {
		a := src.RetentionDays
		if a < 0 {
			a = 0 // -1 means sync all history, not relevant to dedup window
		}
		b := src.MaxErrorRetentionDays
		candidate := a
		if b > candidate {
			candidate = b
		}
		candidate++ // +1 as buffer
		if candidate > window {
			window = candidate
		}
	}
	_, _ = db.Exec(`DELETE FROM sync_state
		WHERE julianday('now') - julianday(created_at) > ?`, window)
}
// runReporter fires at 07:00 local time each day, resets skipped message retry
// flags, drains the error buffer, and appends a digest email to report_label.
// Disabled if report_label is empty.
func runReporter(ctx context.Context, dest *DestClient, destConf DestConfig, db *sql.DB, sources []SourceConfig) {
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

		// Expire old failures and reset skipped_at so messages are retried today.
		// Report is built after reset so it reflects what will be retried.
		resetDailyRetries(db, sources)

		entries := errLog.drain()
		msg := buildReportEmail(destConf.User, entries, db)

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

	debugEnabled = conf.Debug
	if debugEnabled {
		log.Println("debug logging enabled")
	}

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

	// Sweep expired entries from the in-memory dedup cache once a day.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(cacheTTL)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				cacheEvict()
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		runReporter(ctx, dest, conf.Destination, db, conf.Sources)
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
