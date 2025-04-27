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
	"sync"
	"syscall"
	"time"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/nbd-wtf/go-nostr"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Relays               []string `yaml:"relays"`
	FetchIntervalMinutes int      `yaml:"fetch_interval_minutes"`
	DatabasePath         string   `yaml:"database_path"`
}

type StateManager struct {
	db *sql.DB
}

func NewStateManager(db *sql.DB) *StateManager {
	return &StateManager{db: db}
}

func (sm *StateManager) GetLastTimestamp(relayURL string) (time.Time, error) {
	var lastTimestamp sql.NullTime
	query := `SELECT last_timestamp FROM relay_state WHERE relay_url = ?`
	err := sm.db.QueryRow(query, relayURL).Scan(&lastTimestamp)

	if err != nil {
		if err == sql.ErrNoRows {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("failed to query last timestamp for %s: %w", relayURL, err)
	}

	if lastTimestamp.Valid {
		return lastTimestamp.Time, nil
	}
	return time.Time{}, nil
}

func (sm *StateManager) UpdateLastTimestamp(relayURL string, timestamp time.Time) error {

	query := `
		INSERT INTO relay_state (relay_url, last_timestamp) VALUES (?, ?)
		ON CONFLICT(relay_url) DO UPDATE SET last_timestamp = excluded.last_timestamp;
	`
	_, err := sm.db.Exec(query, relayURL, timestamp)
	if err != nil {
		return fmt.Errorf("failed to update last timestamp for %s: %w", relayURL, err)
	}
	return nil
}

type Scheduler struct {
	config     *Config
	stateMgr   *StateManager
	db         *sql.DB
	fetchQueue chan string
	stop       chan struct{}
	wg         sync.WaitGroup
}

func NewScheduler(config *Config, stateMgr *StateManager, db *sql.DB) *Scheduler {

	return &Scheduler{
		config:     config,
		stateMgr:   stateMgr,
		db:         db,
		fetchQueue: make(chan string, len(config.Relays)),
		stop:       make(chan struct{}),
	}
}

func (s *Scheduler) Start() {
	log.Printf("Starting scheduler with interval: %v", s.config.GetFetchIntervalDuration())
	ticker := time.NewTicker(s.config.GetFetchIntervalDuration())

	s.wg.Add(1)

	go func() {
		defer s.wg.Done()
		defer ticker.Stop()

		s.triggerFetchCycle()

		for {
			select {
			case <-ticker.C:
				s.triggerFetchCycle()
			case <-s.stop:
				log.Println("Scheduler received stop signal, shutting down ticker loop...")
				close(s.fetchQueue) // Close queue to signal no more tasks
				return
			}
		}
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for relayURL := range s.fetchQueue {
			fetchRelayEvents(relayURL, s.stateMgr, s.db)
		}
		log.Println("Fetch queue closed, placeholder worker exiting.")
	}()
}

func (s *Scheduler) Stop() {
	log.Println("Stopping scheduler...")

	select {
	case <-s.stop:
		log.Println("Stop signal already sent.")
		return // Already stopping
	default:
		close(s.stop)
	}

	s.wg.Wait()
	log.Println("Scheduler stopped.")
}

func (s *Scheduler) triggerFetchCycle() {
	log.Println("Triggering fetch cycle...")
	if len(s.config.Relays) == 0 {
		log.Println("No relays configured, skipping fetch cycle.")
		return
	}

	for _, relayURL := range s.config.Relays {

		select {
		case s.fetchQueue <- relayURL:
			log.Printf("Queued fetch for: %s", relayURL)
		case <-s.stop:
			log.Println("Stop signal received during fetch cycle trigger, aborting queueing.")
			return
		default:

			select {
			case <-s.stop:
			default:
				log.Printf("Fetch queue is full, skipping queue for: %s", relayURL)
			}
		}
	}
}

func fetchRelayEvents(relayURL string, stateMgr *StateManager, db *sql.DB) {
	log.Printf("Starting fetch task for %s...", relayURL)

	// Use a longer timeout for initial fetches, especially since we're not limiting event counts
	timeoutDuration := 10 * time.Minute
	fetchCtx, cancel := context.WithTimeout(context.Background(), timeoutDuration)
	defer cancel()

	lastTs, err := stateMgr.GetLastTimestamp(relayURL)
	if err != nil {
		log.Printf("Error getting last timestamp for %s: %v", relayURL, err)
		return
	}

	log.Printf("Connecting to relay %s...", relayURL)
	relay, err := nostr.RelayConnect(fetchCtx, relayURL)
	if err != nil {
		log.Printf("Error connecting to %s: %v", relayURL, err)
		return // Cannot proceed if connection fails
	}

	defer func() {
		log.Printf("Closing connection to %s", relayURL)
		if err := relay.Close(); err != nil {
			log.Printf("Error closing relay connection %s: %v", relayURL, err)
		}
	}()

	// Track the latest timestamp successfully processed
	maxTimestamp := lastTs

	// For first-time fetches, we'll use a more robust approach with pagination
	if lastTs.IsZero() {
		log.Printf("First-time fetch for %s", relayURL)

		// We'll use multiple time windows to maximize event collection
		timeWindows := []struct {
			name  string
			since *nostr.Timestamp
			until *nostr.Timestamp
		}{
			// Recent events (last 24 hours)
			{
				name:  "recent",
				since: nostrTimestampPtr(time.Now().Add(-24 * time.Hour)),
				until: nil,
			},
			// Last week
			{
				name:  "last week",
				since: nostrTimestampPtr(time.Now().Add(-7 * 24 * time.Hour)),
				until: nostrTimestampPtr(time.Now().Add(-24 * time.Hour)),
			},
			// Last month
			{
				name:  "last month",
				since: nostrTimestampPtr(time.Now().Add(-30 * 24 * time.Hour)),
				until: nostrTimestampPtr(time.Now().Add(-7 * 24 * time.Hour)),
			},
			// Older events
			{
				name:  "older",
				since: nil,
				until: nostrTimestampPtr(time.Now().Add(-30 * 24 * time.Hour)),
			},
		}

		// Process each time window
		for _, window := range timeWindows {
			log.Printf("Fetching %s events from %s (no limit)", window.name, relayURL)

			// Create filter for this time window - no limit to get all available events
			filter := nostr.Filter{
				Since: window.since,
				Until: window.until,
			}

			// Subscribe with this filter
			sub, err := relay.Subscribe(fetchCtx, nostr.Filters{filter})
			if err != nil {
				log.Printf("Error subscribing to %s for %s events: %v", relayURL, window.name, err)
				continue // Try next window
			}

			// Process events from this window
			windowMaxTs := processSubscription(sub, fetchCtx, relayURL, db)

			// Update max timestamp if needed
			if windowMaxTs.After(maxTimestamp) {
				maxTimestamp = windowMaxTs
			}
		}

		log.Printf("Completed robust initial fetch for %s", relayURL)
	} else {
		// For subsequent fetches, use the regular approach with since filter
		since := lastTs.Unix() + 1
		sinceTimestamp := nostr.Timestamp(since)

		filters := nostr.Filters{{
			Since: &sinceTimestamp,
		}}

		log.Printf("Subscribing to events on %s since %s", relayURL, time.Unix(since, 0).UTC())

		sub, err := relay.Subscribe(fetchCtx, filters)
		if err != nil {
			log.Printf("Error subscribing to %s: %v", relayURL, err)
			return
		}

		// Process events using the regular approach
		subMaxTs := processSubscription(sub, fetchCtx, relayURL, db)

		// Update max timestamp if needed
		if subMaxTs.After(maxTimestamp) {
			maxTimestamp = subMaxTs
		}
	}

	// Update state with max timestamp
	if maxTimestamp.After(lastTs) {
		err = stateMgr.UpdateLastTimestamp(relayURL, maxTimestamp)
		if err != nil {
			log.Printf("Error updating state for %s to %s: %v", relayURL, maxTimestamp.UTC().Format(time.RFC3339), err)
		} else {
			log.Printf("Updated state for %s to %s", relayURL, maxTimestamp.UTC().Format(time.RFC3339))
		}
	} else {
		log.Printf("No new events found, state not updated for %s", relayURL)
	}

	log.Printf("Finished fetch cycle for %s", relayURL)
}

// Helper function to convert time.Time to nostr.Timestamp pointer
func nostrTimestampPtr(t time.Time) *nostr.Timestamp {
	ts := nostr.Timestamp(t.Unix())
	return &ts
}

// Process events from a subscription and return the max timestamp
func processSubscription(sub *nostr.Subscription, ctx context.Context, relayURL string, db *sql.DB) time.Time {
	maxTimestamp := time.Time{}

eventLoop:
	for {
		select {
		case ev := <-sub.Events:
			if ev == nil {
				log.Printf("Received nil event from %s, skipping", relayURL)
				continue
			}

			eventTime := time.Unix(int64(ev.CreatedAt), 0)

			processErr := processEvent(ev, db)
			if processErr != nil {
				log.Printf("Error processing event %s from %s: %v", ev.ID, relayURL, processErr)
			} else {
				if eventTime.After(maxTimestamp) {
					maxTimestamp = eventTime
				}
			}

		case reason := <-sub.ClosedReason:
			log.Printf("Subscription closed for %s: %s", relayURL, reason)
			break eventLoop

		case <-sub.EndOfStoredEvents:
			log.Printf("Received EOSE (End Of Stored Events) from %s", relayURL)
			break eventLoop

		case <-ctx.Done():
			log.Printf("Fetch context cancelled/timed out for %s: %v", relayURL, ctx.Err())
			break eventLoop
		}
	}

	return maxTimestamp
}

func processEvent(event *nostr.Event, db *sql.DB) error {

	// Marshal tags to JSON string
	tagsJSON, err := json.Marshal(event.Tags)
	if err != nil {

		return fmt.Errorf("failed to marshal tags for event %s: %w", event.ID, err)
	}

	_, err = db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO events (id, pubkey, created_at, kind, tags, content)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		event.ID,
		event.PubKey,
		time.Unix(int64(event.CreatedAt), 0),
		event.Kind,
		string(tagsJSON),
		event.Content)

	if err != nil {

		return fmt.Errorf("failed to insert event %s: %w", event.ID, err)
	}

	return nil
}

func main() {
	log.Println("Starting Nostr Relay Monitor...")

	mcpMode := flag.Bool("mcp", false, "Run in MCP stdio mode for direct LLM integration")
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// Load configuration
	config, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	log.Printf("Configuration loaded: %+v", config)
	log.Printf("Fetch interval: %d minutes", config.FetchIntervalMinutes)
	log.Printf("Database path: %s", config.DatabasePath)
	log.Printf("Relays to monitor: %v", config.Relays)

	db, err := initDB(config.DatabasePath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()
	log.Println("Database initialized successfully.")

	stateMgr := NewStateManager(db)
	log.Println("State Manager initialized.")

	analytics := NewAnalytics(db)
	log.Println("Analytics component initialized.")

	mcpServer := NewMCPServer(analytics)

	if *mcpMode {

		if err := mcpServer.StartStdio(); err != nil {
			log.Fatalf("MCP server error: %v", err)
		}
		return
	}
	scheduler := NewScheduler(config, stateMgr, db)
	scheduler.Start()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	log.Println("Setup signal handling. Press Ctrl+C to exit.")

	sig := <-shutdown
	log.Printf("Received signal: %v. Shutting down...", sig)

	scheduler.Stop()

	log.Println("Nostr Relay Monitor stopped gracefully.")
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

	if config.FetchIntervalMinutes <= 0 {
		config.FetchIntervalMinutes = 5 // Default interval if not specified or invalid
		log.Printf("Fetch interval not specified or invalid, using default: %d minutes", config.FetchIntervalMinutes)
	}
	if config.DatabasePath == "" {
		config.DatabasePath = "./nostr_events.db" // Default DB path
		log.Printf("Database path not specified, using default: %s", config.DatabasePath)
	}

	return &config, nil
}

func (c *Config) GetFetchIntervalDuration() time.Duration {
	return time.Duration(c.FetchIntervalMinutes) * time.Minute
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
		content TEXT NOT NULL
	);`

	_, err = db.Exec(createEventsTableSQL)
	if err != nil {
		return nil, fmt.Errorf("failed to create events table: %w", err)
	}

	createStateTableSQL := `
	CREATE TABLE IF NOT EXISTS relay_state (
		relay_url TEXT PRIMARY KEY,
		last_timestamp TIMESTAMP
	);`

	_, err = db.Exec(createStateTableSQL)
	if err != nil {
		return nil, fmt.Errorf("failed to create relay_state table: %w", err)
	}

	log.Println("Database tables ensured.")
	return db, nil
}
