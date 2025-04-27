# Nostr Relay Monitoring Service - Architectural Plan

## 1. Goal

Create a Go service that periodically fetches events from a list of configured Nostr relays and stores them in a DuckDB database for later analysis. The service prioritizes simplicity and efficiency.

## 2. Technology Stack

*   **Language:** Go
*   **Nostr Interaction:** `nbd-wtf/go-nostr`
*   **Database:** DuckDB
*   **DB Driver:** `marcboeker/go-duckdb`
*   **Configuration:** YAML
*   **Scheduling:** Go `time` package (e.g., `time.Ticker`)

## 3. Core Components

*   **Configuration Manager:** Loads relay list, fetch interval, DB path from a file (e.g., `config.yaml`).
*   **Scheduler:** Triggers fetch cycles every 5 minutes.
*   **Relay Fetcher (per relay):** Connects to a relay using `nbd-wtf/go-nostr`, manages the connection, and fetches new events using a `since` filter based on persistent state.
*   **Event Processor & DB Writer:** Receives raw events, parses them into Go structs (excluding the `sig` field), and inserts them into DuckDB using `marcboeker/go-duckdb`.
*   **State Manager:** Persists the `last_timestamp` of the latest processed event (`created_at` field of the last event) for each relay, likely in a dedicated DuckDB table, to manage the `since` filter across restarts.

## 4. Database Schema

### `events` table:

*   `id` (TEXT, PRIMARY KEY): Event hash
*   `pubkey` (TEXT): Author's public key
*   `created_at` (TIMESTAMP): Event creation timestamp
*   `kind` (INTEGER): Event kind
*   `tags` (JSON): Stores the raw JSON array of tags (`[][]string`)
*   `content` (TEXT): Event content

*(Note: The `sig` field is intentionally omitted to save space as it's not required for planned analysis after potential verification)*

### `relay_state` table (Example):

*   `relay_url` (TEXT, PRIMARY KEY): URL of the monitored relay
*   `last_timestamp` (TIMESTAMP): Timestamp of the latest event successfully processed from this relay

## 5. Data Flow Diagram

```mermaid
sequenceDiagram
    participant CfgMgr as Configuration Manager
    participant Sched as Scheduler
    participant StateMgr as State Manager
    participant Fetcher as Relay Fetcher (per relay)
    participant GoNostr as nbd-wtf/go-nostr Lib
    participant Relay as Nostr Relay
    participant DBWriter as DB Writer/Processor
    participant DuckDB as DuckDB Database

    rect rgb(240, 240, 240)
        note over CfgMgr, DuckDB: Initialization Phase
        CfgMgr->>+Sched: Load Config (Relays, Interval, DB Path)
        DBWriter->>+DuckDB: Connect to DB (Path from Config)
        DBWriter->>DBWriter: Ensure 'events' & 'relay_state' Tables Exist (events table without 'sig' column)
        StateMgr->>+DuckDB: Load Initial State from 'relay_state'
        deactivate DuckDB
        deactivate DBWriter
        deactivate CfgMgr
        deactivate StateMgr
    end

    loop Every 5 minutes
        Sched->>+Fetcher: Trigger Fetch Cycle
        Fetcher->>+StateMgr: Get Last Timestamp for Relay X
        StateMgr->>+DuckDB: SELECT last_ts FROM relay_state WHERE relay_url = 'X'
        DuckDB-->>-StateMgr: Return Last Timestamp
        StateMgr-->>-Fetcher: Return Last Timestamp
        Fetcher->>+GoNostr: Prepare REQ filter (since: last_ts + 1)
        GoNostr->>+Relay: Send REQ ["REQ", sub_id, filter]
        Relay-->>-GoNostr: Stream EVENT ["EVENT", sub_id, {...}] (sig verified implicitly/explicitly)
        GoNostr-->>Fetcher: Pass Event Data (excluding sig for storage)
        Fetcher->>+DBWriter: Process Event Data
        DBWriter->>DBWriter: Parse Event JSON to Go Struct (excluding Sig field)
        DBWriter->>+DuckDB: INSERT OR IGNORE INTO events (id, pubkey, created_at, kind, tags, content) VALUES (?, ?, ?, ?, ?, ?)
        DuckDB-->>-DBWriter: Insert Result (Success/Ignored)
        DBWriter->>DBWriter: Track max(created_at) in batch
        DBWriter-->>-Fetcher: Acknowledge Processing
        Relay-->>-GoNostr: Stream EOSE ["EOSE", sub_id]
        GoNostr-->>Fetcher: Pass EOSE Signal
        Fetcher->>+StateMgr: Update Last Timestamp for Relay X with max(created_at)
        StateMgr->>+DuckDB: UPDATE relay_state SET last_ts = ... WHERE relay_url = 'X'
        DuckDB-->>-StateMgr: Update OK
        StateMgr-->>-Fetcher: Acknowledge State Update
        deactivate StateMgr
        deactivate DBWriter
        deactivate GoNostr
        deactivate Relay
        deactivate Fetcher
        deactivate DuckDB
    end
```

## 6. Key Considerations

*   **Error Handling:** Implement robust logging and retry logic for network/DB issues.
*   **Service Monitoring:** Add basic health checks for the service itself.