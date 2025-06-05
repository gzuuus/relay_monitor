package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/nbd-wtf/go-nostr"
	"gopkg.in/yaml.v3"
)

const InactivityFlushInterval = 15 * time.Second

type RelayMonitor struct {
	URL                string
	Events             chan *nostr.Event
	EventBuffer        []*nostr.Event
	BufferMtx          sync.Mutex
	DB                 *sql.DB
	BatchInterval      time.Duration
	BlacklistedPubkeys []string
	reconnectCh        chan struct{}
	stopCh             chan struct{}
	eventSignalCh      chan struct{}
	wg                 sync.WaitGroup
}

func NewRelayMonitor(url string, db *sql.DB, batchInterval time.Duration, blacklistedPubkeys []string) *RelayMonitor {
	return &RelayMonitor{
		URL:                url,
		Events:             make(chan *nostr.Event, 100),
		EventBuffer:        make([]*nostr.Event, 0),
		DB:                 db,
		BatchInterval:      batchInterval,
		BlacklistedPubkeys: blacklistedPubkeys,
		reconnectCh:        make(chan struct{}),
		stopCh:             make(chan struct{}),
		eventSignalCh:      make(chan struct{}, 1),
	}
}

func (rm *RelayMonitor) Start() {
	log.Printf("[MONITOR] Starting monitor for relay: %s", rm.URL)

	rm.wg.Add(1)
	go rm.subscribeLoop()

	rm.wg.Add(1)
	go rm.processEvents()

	rm.wg.Add(1)
	go rm.flushBufferPeriodically()
}

func (rm *RelayMonitor) Stop() {
	log.Printf("[MONITOR] Stopping monitor for relay: %s", rm.URL)
	close(rm.stopCh)
	rm.wg.Wait()
	log.Printf("[MONITOR] Monitor for relay %s stopped.", rm.URL)
}

func (rm *RelayMonitor) subscribeLoop() {
	defer rm.wg.Done()
	backoffTime := 1 * time.Second
	maxBackoffTime := 60 * time.Second

	for {
		select {
		case <-rm.stopCh:
			log.Printf("[MONITOR] subscribeLoop for %s received stop signal.", rm.URL)
			return
		default:

		}

		log.Printf("[MONITOR] Attempting to connect to relay: %s", rm.URL)
		var relay *nostr.Relay
		var sub *nostr.Subscription

		ctx, cancel := context.WithCancel(context.Background())

		connectedRelay, err := nostr.RelayConnect(ctx, rm.URL)
		if err != nil {
			log.Printf("[ERROR] Failed to connect to %s: %v. Retrying in %v...", rm.URL, err, backoffTime)
			cancel()
			time.Sleep(backoffTime)
			backoffTime = min(backoffTime*2, maxBackoffTime)
			continue
		}
		relay = connectedRelay

		log.Printf("[MONITOR] Connected to %s. Subscribing to firehose...", rm.URL)
		connectedSub, err := relay.Subscribe(ctx, nostr.Filters{{}})
		if err != nil {
			log.Printf("[ERROR] Failed to subscribe to %s: %v. Retrying in %v...", rm.URL, err, backoffTime)
			cancel()
			if relay != nil {
				relay.Close()
			}
			time.Sleep(backoffTime)
			backoffTime = min(backoffTime*2, maxBackoffTime)
			continue
		}
		sub = connectedSub

		log.Printf("[MONITOR] Subscribed to firehose for %s.", rm.URL)
		backoffTime = 1 * time.Second

	eventLoop:
		for {
			select {
			case ev := <-sub.Events:
				if ev == nil {
					log.Printf("[MONITOR] Received nil event from %s, skipping", rm.URL)
					continue
				}
				log.Printf("[MONITOR] Received event from %s: %v", rm.URL, ev.ID)
				rm.Events <- ev
			case reason := <-sub.ClosedReason:
				log.Printf("[MONITOR] Subscription for %s closed: %s. Reconnecting...", rm.URL, reason)
				break eventLoop
			case <-rm.reconnectCh:
				log.Printf("[MONITOR] Forced reconnect for %s.", rm.URL)
				break eventLoop
			case <-rm.stopCh:
				log.Printf("[MONITOR] subscribeLoop for %s received stop signal, closing subscription.", rm.URL)
				cancel()
				return
			}
		}
		// If eventLoop breaks for any reason other than rm.stopCh:
		cancel()
		if relay != nil {
			relay.Close()
		}
	}
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (rm *RelayMonitor) processEvents() {
	defer rm.wg.Done()
	for {
		select {
		case ev := <-rm.Events:
			if slices.Contains(rm.BlacklistedPubkeys, ev.PubKey) {
				log.Printf("[FILTER] skipping event %s from blacklisted pubkey %s", ev.ID, ev.PubKey)
				continue
			}
			rm.BufferMtx.Lock()
			rm.EventBuffer = append(rm.EventBuffer, ev)
			rm.BufferMtx.Unlock()
			select {
			case rm.eventSignalCh <- struct{}{}:
			default:
			}
		case <-rm.stopCh:
			log.Printf("[MONITOR] processEvents for %s received stop signal.", rm.URL)
			return
		}
	}
}

func (rm *RelayMonitor) flushBufferPeriodically() {
	defer rm.wg.Done()
	ticker := time.NewTicker(rm.BatchInterval)
	defer ticker.Stop()
	inactivityTimer := time.NewTimer(InactivityFlushInterval)
	defer inactivityTimer.Stop()

	for {
		select {
		case <-ticker.C:
			rm.doFlush()
			inactivityTimer.Reset(InactivityFlushInterval)
		case <-rm.eventSignalCh:
			inactivityTimer.Stop()
			inactivityTimer.Reset(InactivityFlushInterval)
		case <-inactivityTimer.C:
			if len(rm.EventBuffer) > 0 {
				log.Printf("[MONITOR] No new events for %s, flushing due to inactivity.", rm.URL)
				rm.doFlush()
			}
		case <-rm.stopCh:
			log.Printf("[MONITOR] flushBufferPeriodically for %s received stop signal. Flushing remaining events...", rm.URL) // log shutdown and final flush
			rm.BufferMtx.Lock()                                                                                               // acquire lock for safe buffer access on shutdown
			eventsToFlush := rm.EventBuffer
			rm.EventBuffer = make([]*nostr.Event, 0)
			rm.BufferMtx.Unlock()

			if len(eventsToFlush) > 0 {
				log.Printf("[MONITOR] Flushing %d remaining events to DuckDB for %s during shutdown...", len(eventsToFlush), rm.URL)
				err := insertEventsBatch(rm.DB, eventsToFlush, rm.URL)
				if err != nil {
					log.Printf("[ERROR] Failed to flush remaining events to DuckDB for %s: %v", rm.URL, err)
				} else {
					log.Printf("[MONITOR] Successfully flushed %d remaining events for %s.", len(eventsToFlush), rm.URL)
				}
			}
			return
		}
	}
}

func insertEventsBatch(db *sql.DB, events []*nostr.Event, relayURL string) error {
	if len(events) == 0 {
		return nil
	}

	dbAccessMutex.Lock()
	defer dbAccessMutex.Unlock()

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
        INSERT OR IGNORE INTO events (id, pubkey, created_at, kind, tags, content, relay_url)
        VALUES (?, ?, ?, ?, ?, ?, ?)
    `)
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, event := range events {
		tagsJSON, err := json.Marshal(event.Tags)
		if err != nil {
			log.Printf("[ERROR] Failed to marshal tags for event %s: %v", event.ID, err)
			continue
		}

		_, err = stmt.Exec(
			event.ID,
			event.PubKey,
			time.Unix(int64(event.CreatedAt), 0),
			event.Kind,
			string(tagsJSON),
			event.Content,
			relayURL,
		)
		if err != nil {
			log.Printf("[ERROR] Failed to insert event %s into batch: %v", event.ID, err)
		}
	}

	return tx.Commit()
}

type Config struct {
	Relays                    []string `yaml:"relays"`
	BatchFlushIntervalMinutes int      `yaml:"batch_flush_interval_minutes"`
	DatabasePath              string   `yaml:"database_path"`
	BlacklistedPubkeys        []string `yaml:"blacklisted_pubkeys"`
}

func (rm *RelayMonitor) doFlush() {
	rm.BufferMtx.Lock()
	eventsToFlush := rm.EventBuffer
	rm.EventBuffer = make([]*nostr.Event, 0)
	rm.BufferMtx.Unlock()

	if len(eventsToFlush) > 0 {
		log.Printf("[MONITOR] Flushing %d events to DuckDB for %s...", len(eventsToFlush), rm.URL)
		err := insertEventsBatch(rm.DB, eventsToFlush, rm.URL)
		if err != nil {
			log.Printf("[ERROR] Failed to flush events to DuckDB for %s: %v", rm.URL, err)
		} else {
			log.Printf("[MONITOR] Successfully flushed %d events for %s.", len(eventsToFlush), rm.URL)
		}
	}
}

func runApp() error {
	mcpMode := flag.Bool("mcp", false, "Run in MCP stdio mode for direct LLM integration")
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// Configure logging
	if *mcpMode {
		log.SetOutput(os.Stderr) // Redirect logs when in MCP mode to avoid interfering with stdio communication
	}
	log.Println("[MAIN] starting nostr relay monitor")

	config, err := loadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	log.Printf("[CONFIG] loaded: %+v", config)
	log.Printf("[CONFIG] batch flush interval: %d minutes", config.BatchFlushIntervalMinutes)
	log.Printf("[CONFIG] database path: %s", config.DatabasePath)
	log.Printf("[CONFIG] relays to monitor: %v", config.Relays)

	db, err := initDB(config.DatabasePath)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer db.Close()

	log.Println("[MAIN] database initialized")

	// Initialize and start RelayMonitors
	var monitors []*RelayMonitor
	for _, relayURL := range config.Relays {
		monitor := NewRelayMonitor(relayURL, db, time.Duration(config.BatchFlushIntervalMinutes)*time.Minute, config.BlacklistedPubkeys)
		monitors = append(monitors, monitor)
		monitor.Start()
	}
	log.Printf("[MAIN] Started %d relay monitors.", len(monitors))

	analytics := NewAnalytics(db) // Assuming NewAnalytics can be called here with the db
	log.Println("[MAIN] analytics component initialized")

	mcpServer := NewMCPServer(analytics)

	if *mcpMode {
		log.Println("[MAIN] starting in MCP mode with background monitors")
		if err := mcpServer.StartStdio(); err != nil {
			return fmt.Errorf("MCP server error: %w", err)
		}
		return nil // Exit immediately after MCP server stops
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	log.Println("[MAIN] signal handling setup, press Ctrl+C to exit")

	sig := <-shutdown
	log.Printf("[MAIN] received signal: %v, shutting down", sig)

	// Stop all monitors gracefully
	for _, monitor := range monitors {
		monitor.Stop()
	}

	log.Println("[MAIN] stopped gracefully")
	return nil
}

func main() {
	if err := runApp(); err != nil {
		log.Fatalf("[ERROR] application failed: %v", err)
	}
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	if config.BatchFlushIntervalMinutes <= 0 {
		config.BatchFlushIntervalMinutes = 5 // Default interval if not specified or invalid
		log.Printf("Batch flush interval not specified or invalid, using default: %d minutes", config.BatchFlushIntervalMinutes)
	}
	if config.DatabasePath == "" {
		config.DatabasePath = "./nostr_events.db" // Default DB path
		log.Printf("Database path not specified, using default: %s", config.DatabasePath)
	}

	return &config, nil
}

func initDB(dbPath string) (*sql.DB, error) {

	db, err := sql.Open("duckdb", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err = db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	createEventsTableSQL := `
	CREATE TABLE IF NOT EXISTS events (
		id TEXT PRIMARY KEY,
		pubkey TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL,
		kind INTEGER NOT NULL,
		tags JSON,
		content TEXT NOT NULL,
		relay_url TEXT
	);`

	_, err = db.Exec(createEventsTableSQL)
	if err != nil {
		return nil, fmt.Errorf("failed to create events table: %w", err)
	}

	log.Println("Database tables ensured.")
	return db, nil
}
