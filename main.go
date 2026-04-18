package main

import (
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
	idle "github.com/emersion/go-imap-idle"
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
	ReportLabel string `json:"report_label"`
	SkipVerify  bool   `json:"skip_verify"`
	MarkAsRead  bool   `json:"mark_as_read"`

	// Gmail OAuth2 — set provider:"gmail" and supply these instead of pass.
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
	RetentionDays int         `json:"retention_days"`
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

// ---------- In-memory cache ----------

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

// loadOAuthToken reads a cached token from disk, or runs the interactive
// consent flow once to obtain one and saves it for future runs.
func loadOAuthToken(credFile, tokenFile string) (*oauth2.Token, *oauth2.Config, error) {
	b, err := os.ReadFile(credFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read credentials %q: %w", credFile, err)
	}

	cfg, err := google.ConfigFromJSON(b, "https://mail.google.com/")
	if err != nil {
		return nil, nil, fmt.Errorf("parse credentials: %w", err)
	}

	// Use cached token if present and parseable.
	if data, err := os.ReadFile(tokenFile); err == nil {
		var tok oauth2.Token
		if json.Unmarshal(data, &tok) == nil {
			return &tok, cfg, nil
		}
	}

	// First-run interactive flow.
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

// freshAccessToken refreshes the token if expired and persists the new one.
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

// dial opens a raw IMAP connection (SSL, STARTTLS, or plain).
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

// connectAndLogin dials and authenticates, supporting both password and Gmail OAuth2.
// Returns the connected client plus (optionally) the oauth config and token for later refresh.
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
		saslClient := sasl.NewXoauth2Client(user, accessToken)
		if err := c.Authenticate(saslClient); err != nil {
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

// DestClient holds a persistent, auto-reconnecting IMAP append connection.
// Append failures are retried with exponential backoff.
type DestClient struct {
	conf DestConfig

	mu       sync.Mutex
	c        *client.Client
	oauthCfg *oauth2.Config
	oauthTok *oauth2.Token
}

// redial (re)establishes the connection. Caller must hold mu.
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
	d.c = c
	d.oauthCfg = oCfg
	d.oauthTok = oTok
	return nil
}

// ensureConn dials if there is no live connection. Caller must hold mu.
func (d *DestClient) ensureConn() error {
	if d.c != nil {
		return nil
	}
	return d.redial()
}

// Append delivers a message to label, retrying on any transient failure.
func (d *DestClient) Append(ctx context.Context, label string, r imap.Literal) error {
	flags := []string{}
	if d.conf.MarkAsRead {
		flags = []string{imap.SeenFlag}
	}

	for attempt := 0; attempt < maxDestAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		d.mu.Lock()
		connErr := d.ensureConn()
		if connErr != nil {
			d.mu.Unlock()
			wait := backoff(attempt)
			log.Printf("dest connect error (attempt %d/%d): %v — retry in %s",
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
			// Invalidate so ensureConn re-dials next iteration.
			_ = d.c.Logout()
			d.c = nil
		}
		d.mu.Unlock()

		if appendErr == nil {
			return nil
		}

		wait := backoff(attempt)
		log.Printf("dest append error (attempt %d/%d): %v — retry in %s",
			attempt+1, maxDestAttempts, appendErr, wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return fmt.Errorf("dest: exhausted %d attempts appending to %q", maxDestAttempts, label)
}

// ---------- Sync ----------

func syncFolder(ctx context.Context, src *client.Client, dest *DestClient, db *sql.DB, user, folder, label string) {
	key := user + ":" + folder

	var lastUID uint32
	_ = db.QueryRow("SELECT last_uid FROM folder_offsets WHERE account_folder = ?", key).Scan(&lastUID)

	criteria := imap.NewSearchCriteria()
	if lastUID > 0 {
		set := new(imap.SeqSet)
		set.AddRange(lastUID+1, 0)
		criteria.Uid = set
	}

	uids, err := src.UidSearch(criteria)
	if err != nil {
		log.Println("UID search error:", err)
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

		// Fetch envelope to get Message-ID cheaply.
		envCh := make(chan *imap.Message, 1)
		go src.UidFetch(set, []imap.FetchItem{imap.FetchEnvelope}, envCh)
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

		// Fetch full RFC822 body.
		section := &imap.BodySectionName{BodyPartName: imap.RFC822Name}
		bodyCh := make(chan *imap.Message, 1)
		go src.UidFetch(set, []imap.FetchItem{section.FetchItem()}, bodyCh)
		body := <-bodyCh

		if body == nil {
			continue
		}

		if err := dest.Append(ctx, label, body.GetReader(section)); err != nil {
			log.Printf("append failed for msg %s: %v", id, err)
			continue // skip this message this cycle; it will be retried next sync
		}

		_, _ = db.Exec("INSERT INTO sync_state (msgid) VALUES (?)", id)
		cacheAdd(id)
	}

	if maxUID > 0 {
		_, _ = db.Exec("INSERT OR REPLACE INTO folder_offsets VALUES (?, ?)", key, maxUID)
	}
}

// ---------- Source worker ----------

func monitorSource(ctx context.Context, src SourceConfig, destConf DestConfig, db *sql.DB) {
	dest := &DestClient{conf: destConf}
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
			log.Printf("source connect failed (%s, attempt %d): %v — retry in %s",
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
		idleClient := idle.NewClient(c)

		for _, m := range src.Mappings {
			if ctx.Err() != nil {
				c.Logout()
				return
			}

			if _, err := c.Select(m.From, true); err != nil {
				log.Printf("select %q failed: %v — reconnecting", m.From, err)
				c.Logout()
				goto reconnect
			}

		idleLoop:
			for {
				if ctx.Err() != nil {
					c.Logout()
					return
				}

				syncFolder(ctx, c, dest, db, src.User, m.From, m.To)

				updates := make(chan client.Update, 10)
				c.Updates = updates

				stop := make(chan struct{})
				idleDone := make(chan error, 1)
				go func() { idleDone <- idleClient.IdleWithFallback(stop, 0) }()

				select {
				case <-updates:
					close(stop)
					<-idleDone

				case <-time.After(10 * time.Minute):
					close(stop)
					<-idleDone
					if err := c.Noop(); err != nil {
						log.Printf("noop failed on %s — reconnecting: %v", src.Host, err)
						c.Logout()
						goto reconnect
					}

				case <-ctx.Done():
					close(stop)
					<-idleDone
					c.Logout()
					return
				}

				_ = idleLoop
			}
		}

	reconnect:
		log.Printf("reconnecting source: %s", src.Host)
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

	var wg sync.WaitGroup
	for _, src := range conf.Sources {
		wg.Add(1)
		go func(s SourceConfig) {
			defer wg.Done()
			monitorSource(ctx, s, conf.Destination, db)
		}(src)
	}

	<-ctx.Done()
	log.Println("shutdown signal received")
	wg.Wait()
	log.Println("clean exit")
}
