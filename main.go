package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
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
	"github.com/emersion/go-imap-idle"
	"github.com/emersion/go-imap/client"
	_ "github.com/mattn/go-sqlite3"
)

type Config struct {
	Destination DestConfig   `json:"destination"`
	Sources     []SourceConfig `json:"sources"`
}

type DestConfig struct {
	Host, User, Pass, Security, ReportLabel string
	SkipVerify, MarkAsRead                  bool `json:"skip_verify"`
}

type SourceConfig struct {
	Host, User, Pass, Security string
	SkipVerify                 bool `json:"skip_verify"`
	RetentionDays              int  `json:"retention_days"`
	Mappings                   []FolderMap `json:"mappings"`
}

type FolderMap struct {
	From, To string
}

type cacheEntry struct {
	ts time.Time
}

var (
	msgCache = make(map[string]cacheEntry)
	cacheMux sync.Mutex
	cacheTTL = 24 * time.Hour
)

// ---------- DB ----------

func initDB(path string) *sql.DB {
	db, err := sql.Open("sqlite3", path+"?_journal=WAL&_sync=1")
	if err != nil {
		log.Fatal("DB open failed:", err)
	}

	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS folder_offsets (
		account_folder TEXT PRIMARY KEY,
		last_uid INTEGER
	);
	CREATE TABLE IF NOT EXISTS sync_state (
		msgid TEXT PRIMARY KEY,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`)
	if err != nil {
		log.Fatal("DB init failed:", err)
	}

	return db
}

// ---------- Connection ----------

func connect(host, security string, skipVerify bool) (*client.Client, error) {
	tlsConfig := &tls.Config{InsecureSkipVerify: skipVerify}

	if strings.ToLower(security) == "ssl" {
		return client.DialTLS(host, tlsConfig)
	}

	c, err := client.Dial(host)
	if err != nil {
		return nil, err
	}

	if strings.ToLower(security) == "tls" {
		if err := c.StartTLS(tlsConfig); err != nil {
			return nil, err
		}
	}
	return c, nil
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

// ---------- Cache ----------

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

// ---------- Destination Client ----------

type DestClient struct {
	conf DestConfig
	mu   sync.Mutex
	c    *client.Client
}

func (d *DestClient) ensure() error {
	if d.c != nil {
		return nil
	}

	c, err := connect(d.conf.Host, d.conf.Security, d.conf.SkipVerify)
	if err != nil {
		return err
	}

	if err := c.Login(d.conf.User, d.conf.Pass); err != nil {
		return err
	}

	d.c = c
	return nil
}

func (d *DestClient) append(label string, r imap.Literal) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.ensure(); err != nil {
		return err
	}

	flags := []string{}
	if d.conf.MarkAsRead {
		flags = []string{imap.SeenFlag}
	}

	err := d.c.Append(label, flags, time.Now(), r)
	if err != nil {
		d.c.Logout()
		d.c = nil
	}
	return err
}

// ---------- Sync ----------

func syncFolder(src *client.Client, dest *DestClient, db *sql.DB, user, folder, label string) {
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
		if uid > maxUID {
			maxUID = uid
		}

		set := new(imap.SeqSet)
		set.AddNum(uid)

		msgs := make(chan *imap.Message, 1)
		go src.UidFetch(set, []imap.FetchItem{imap.FetchEnvelope}, msgs)
		msg := <-msgs

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

		bodyMsgs := make(chan *imap.Message, 1)
		section := &imap.BodySectionName{BodyPartName: imap.RFC822Name}

		go src.UidFetch(set, []imap.FetchItem{section.FetchItem()}, bodyMsgs)
		body := <-bodyMsgs

		if body == nil {
			continue
		}

		err := dest.append(label, body.GetReader(section))
		if err != nil {
			log.Println("append failed:", err)
			continue
		}

		_, _ = db.Exec("INSERT INTO sync_state (msgid) VALUES (?)", id)
		cacheAdd(id)
	}

	if maxUID > 0 {
		_, _ = db.Exec("INSERT OR REPLACE INTO folder_offsets VALUES (?, ?)", key, maxUID)
	}
}

// ---------- Worker ----------

func monitorSource(ctx context.Context, src SourceConfig, destConf DestConfig, db *sql.DB) {
	dest := &DestClient{conf: destConf}

	attempt := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		c, err := connect(src.Host, src.Security, src.SkipVerify)
		if err != nil {
			sleep := backoff(attempt)
			log.Println("connect failed:", err, "retrying in", sleep)
			time.Sleep(sleep)
			attempt++
			continue
		}

		if err := c.Login(src.User, src.Pass); err != nil {
			log.Println("login failed:", err)
			c.Logout()
			time.Sleep(backoff(attempt))
			continue
		}

		attempt = 0
		idleClient := idle.NewClient(c)

		for _, m := range src.Mappings {
			for {
				select {
				case <-ctx.Done():
					c.Logout()
					return
				default:
				}

				syncFolder(c, dest, db, src.User, m.From, m.To)

				updates := make(chan client.Update, 10)
				c.Updates = updates

				stop := make(chan struct{})
				go idleClient.Idle(stop)

				select {
				case <-updates:
					close(stop)
				case <-time.After(10 * time.Minute):
					close(stop)
					if err := c.Noop(); err != nil {
						log.Println("noop failed, reconnecting")
						c.Logout()
						goto reconnect
					}
				case <-ctx.Done():
					close(stop)
					c.Logout()
					return
				}
			}
		}

	reconnect:
		log.Println("reconnecting source:", src.Host)
		c.Logout()
	}
}

// ---------- MAIN ----------

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	file, err := os.Open("config.json")
	if err != nil {
		log.Fatal("config load failed:", err)
	}

	var conf Config
	if err := json.NewDecoder(file).Decode(&conf); err != nil {
		log.Fatal("config parse failed:", err)
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
