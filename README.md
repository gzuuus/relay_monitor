# Nostr Relay Monitor

A Go application for monitoring Nostr relays, collecting events, and providing analytics through a Model Context Protocol (MCP) interface.

## Overview

Nostr Relay Monitor connects to specified Nostr relays, collects events, and stores them in a DuckDB database. It provides analytics capabilities through an MCP interface.

The application supports:
- Monitoring multiple relays simultaneously
- Incremental event collection after initial sync
- Analytics through MCP tools

## Installation

### Prerequisites

- Go 1.18 or higher

### Setup

1. Clone the repository:
   ```bash
   git clone https://github.com/gzuuus/relay_monitor.git
   cd relay_monitor
   ```

2. Install dependencies:
   ```bash
   go mod download
   ```

3. Copy the example configuration:
   ```bash
   cp config.example.yaml config.yaml
   ```

4. Edit `config.yaml` to specify your relays and settings:
   ```yaml
   # List of Nostr relays to monitor
   relays:
     - wss://relay1.com
     - wss://relay2.com
     - wss://relay3.com

   # Interval between fetch cycles (in minutes)
   fetch_interval_minutes: 5

   # Path to the DuckDB database file
   database_path: ./nostr_events.db
   ```

## Usage

### Running the Application

To run the application in standard mode (collecting events from relays):

```bash
go run .
```

To run in MCP mode (for AI assistant integration):

```bash
go run . --mcp
```

### Using with MCP Inspector

For development and testing, you can use the MCP Inspector:

```bash
npx @modelcontextprotocol/inspector go run . --mcp
```

This will start the MCP Inspector interface at http://127.0.0.1:6274, allowing you to test the MCP tools.

## MCP Tools

The application exposes the following MCP tools:

### event_count_by_kind

Get count of Nostr events grouped by kind with time filter.

**Parameters:**
- `time_filter` (string, optional): Time filter to apply (last_24_hours, last_week, last_month, all_time)

**Example Response:**
```json
[
  {
    "kind": 1,
    "count": 1250
  },
  {
    "kind": 3,
    "count": 78
  }
]
```

### top_authors

Get the most active authors (pubkeys) by event count with optional kind filtering.

**Parameters:**
- `kind` (number, optional): Optional event kind to filter by
- `limit` (number, optional): Maximum number of authors to return
- `time_filter` (string, optional): Time filter to apply (last_24_hours, last_week, last_month, all_time)

**Example Response:**
```json
[
  {
    "pubkey": "a745f8d6c21e7a3256b8ca1b6c9b12ac08d84987b981a25e6ac5fca55efee4b6",
    "count": 42
  },
  {
    "pubkey": "32e1827635450ebb3c5a7d12c1f8e7b2b514439ac10a67eef3d9fd9c5c68e245",
    "count": 36
  }
]
```

### event_count_by_time

Get event counts aggregated by time periods with optional kind filtering.

**Parameters:**
- `interval` (string, required): Time interval for aggregation (hour, day, week, month)
- `limit` (number, optional): Maximum number of time periods to return
- `kind` (number, optional): Optional event kind to filter by

**Example Response:**
```json
{
  "2025-04-26": 156,
  "2025-04-27": 203,
  "2025-04-28": 187
}
```

### relay_stats

Get statistics about the relay monitoring.

**Parameters:**
- `time_filter` (string, optional): Time filter to apply (last_24_hours, last_week, last_month, all_time)

**Example Response:**
```json
{
  "relays": {
    "wss://relay1.com": "2025-04-27T15:04:14Z",
    "wss://relay2.com": "2025-04-27T15:02:23Z"
  },
  "total_events": 1567,
  "unique_authors": 89,
  "unique_kinds": 5,
  "time_filter": "last_week"
}
```

## Architecture

The application consists of several components:

- **Main**: Initializes the application, handles configuration, and sets up components
- **Scheduler**: Manages the timed fetching process for relays
- **StateManager**: Tracks the last processed timestamp for each relay
- **Analytics**: Provides query methods for analyzing the collected data
- **MCPServer**: Exposes analytics functions as MCP tools

## Database Schema

The application uses DuckDB with the following schema:

### events
- `id` (TEXT): The event ID
- `pubkey` (TEXT): The author's public key
- `created_at` (TIMESTAMP): When the event was created
- `kind` (INTEGER): The event kind
- `tags` (JSON): Event tags
- `content` (TEXT): Event content

### relay_state
- `relay_url` (TEXT): The relay URL
- `last_timestamp` (TIMESTAMP): The timestamp of the last processed event

## License

[MIT License](LICENSE)
