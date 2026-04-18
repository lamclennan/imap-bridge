package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
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
	From string `json:"from"`
	To   string `json:"to"`
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
		if err := c.Authenticate(sasl.NewXoauth2Client(user, accessToken)); err != nil {
			c.Logout()
			return nil, nil, nil, fmt.Errorf("gmail xoauth2: %w", err)
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
	for attempt := 0; attempt < maxDestAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
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
				return ctx.Err()
			}
			continue
		}

		appendErr := d.c.Append(label, flags, time.Now(), r)
		if appendErr != nil {
			_ = d.c.Logout()
			d.c = nil
		}
		d.mu.Unlock()

		if appendErr == nil {
			return nil
		}

		wait := backoff(attempt)
		errLog.add("dest append error (attempt %d/%d): %v — retry in %s",
			attempt+1, maxDestAttempts, appendErr, wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return fmt.Errorf("dest: exhausted %d attempts appending to %q", maxDestAttempts, label)
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

// syncFolder fetches unseen messages from src, mirrors their \Seen flag, and
// appends them to the destination label.
func syncFolder(ctx context.Context, c *client.Client, dest *DestClient, db *sql.DB, user, folder, label string) {
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
		if id == "" || cacheSeen(id) {
			continue
		}

		var exists string
		_ = db.QueryRow("SELECT msgid FROM sync_state WHERE msgid = ?", id).Scan(&exists)
		if exists != "" {
			cacheAdd(id)
			continue
		}

		// Mirror \Seen from source to destination.
		destFlags := []string{}
		for _, f := range msg.Flags {
			if f == imap.SeenFlag {
				destFlags = append(destFlags, imap.SeenFlag)
				break
			}
		}

		// Fetch full message body using EntireSpecifier for broad server compatibility.
		section := &imap.BodySectionName{Specifier: imap.EntireSpecifier}
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

		if err := dest.Append(ctx, label, destFlags, literal); err != nil {
			errLog.add("append failed for msg %s (%s/%s): %v", id, user, folder, err)
			continue
		}

		_, _ = db.Exec("INSERT INTO sync_state (msgid) VALUES (?)", id)
		cacheAdd(id)
	}

	if maxUID > 0 {
		_, _ = db.Exec("INSERT OR REPLACE INTO folder_offsets VALUES (?, ?)", key, maxUID)
	}
}

// ---------- Source worker ----------

// monitorSource manages the full lifecycle for one source account.
// Connects, syncs all mappings, watches for new mail, reconnects on failure.
// No goto — uses a clean helper function for the connected phase.
func monitorSource(ctx context.Context, src SourceConfig, dest *DestClient, db *sql.DB) {
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

		if cleanExit := runMappings(ctx, c, src, dest, db); cleanExit {
			return
		}
		log.Printf("connection lost to %s — reconnecting", src.Host)
	}
}

// runMappings runs the sync+polling loop for all folder mappings on an open connection.
// Returns true if the exit was due to ctx cancellation (clean shutdown),
// false if the connection broke and a reconnect is needed.
func runMappings(ctx context.Context, c *client.Client, src SourceConfig, dest *DestClient, db *sql.DB) (cleanExit bool) {
	defer c.Logout()

	for _, m := range src.Mappings {
		if ctx.Err() != nil {
			return true
		}

		// Select read-write (false) so pruneFolder can expunge if needed.
		if _, err := c.Select(m.From, false); err != nil {
			errLog.add("select %q failed on %s: %v — reconnecting", m.From, src.Host, err)
			return false
		}

		for {
			if ctx.Err() != nil {
				return true
			}

			syncFolder(ctx, c, dest, db, src.User, m.From, m.To)

			if src.RetentionDays > 0 {
				pruneFolder(ctx, c, src.User, m.From, src.RetentionDays)
			}

			updates := make(chan client.Update, 10)
			c.Updates = updates

			select {
			case <-updates:
				// New mail notification — re-sync immediately.

			case <-time.After(10 * time.Minute):
				if err := c.Noop(); err != nil {
					errLog.add("noop failed on %s: %v — reconnecting", src.Host, err)
					return false
				}

			case <-ctx.Done():
				return true
			}

			// Clear updates channel for next iteration
			c.Updates = nil
		}
	}

	return true
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

	wg.Add(1)
	go func() {
		defer wg.Done()
		runReporter(ctx, dest, conf.Destination)
	}()

	for _, src := range conf.Sources {
		wg.Add(1)
		go func(s SourceConfig) {
			defer wg.Done()
			monitorSource(ctx, s, dest, db)
		}(src)
	}

	<-ctx.Done()
	log.Println("shutdown signal received")
	wg.Wait()
	log.Println("clean exit")
}
