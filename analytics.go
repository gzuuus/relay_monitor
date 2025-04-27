package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Analytics struct {
	db *sql.DB
}

func NewAnalytics(db *sql.DB) *Analytics {
	return &Analytics{db: db}
}

func (a *Analytics) EventCountByKind(ctx context.Context) (map[int]int, error) {

	return a.EventCountByKindWithTimeFilter(ctx, AllTime)
}

type AuthorStats struct {
	PubKey string `json:"pubkey"`
	Count  int    `json:"count"`
}

func (a *Analytics) TopAuthors(ctx context.Context, limit int, filter TimeFilter) ([]AuthorStats, error) {
	timeCondition, args := getTimeFilterCondition(filter)
	query := fmt.Sprintf(`
		SELECT pubkey, COUNT(*) as count
		FROM events
		WHERE %s
		GROUP BY pubkey
		ORDER BY count DESC
		LIMIT ?
	`, timeCondition)

	// Combine args with limit
	queryArgs := []any{}
	if args != nil {
		queryArgs = append(queryArgs, args...)
	}
	queryArgs = append(queryArgs, limit)

	rows, err := a.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to query top authors: %w", err)
	}
	defer rows.Close()

	var results []AuthorStats

	for rows.Next() {
		var pubkey string
		var count int
		if err := rows.Scan(&pubkey, &count); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		results = append(results, AuthorStats{PubKey: pubkey, Count: count})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	return results, nil
}

func (a *Analytics) TopAuthorsByKind(ctx context.Context, kind int, limit int, filter TimeFilter) ([]AuthorStats, error) {
	timeCondition, args := getTimeFilterCondition(filter)
	query := fmt.Sprintf(`
		SELECT pubkey, COUNT(*) as count
		FROM events
		WHERE kind = ? AND %s
		GROUP BY pubkey
		ORDER BY count DESC
		LIMIT ?
	`, timeCondition)

	// Combine args with kind and limit
	queryArgs := []any{kind}
	if args != nil {
		queryArgs = append(queryArgs, args...)
	}
	queryArgs = append(queryArgs, limit)

	rows, err := a.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to query top authors for kind %d: %w", kind, err)
	}
	defer rows.Close()

	var results []AuthorStats

	for rows.Next() {
		var pubkey string
		var count int
		if err := rows.Scan(&pubkey, &count); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		results = append(results, AuthorStats{PubKey: pubkey, Count: count})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	return results, nil
}

func (a *Analytics) EventCountByTimeRange(ctx context.Context, interval string, limit int, kind *int) (map[string]int, error) {
	var timeFormat string

	switch interval {
	case "hour":
		timeFormat = "strftime('%Y-%m-%d %H:00', created_at)"
	case "day":
		timeFormat = "strftime('%Y-%m-%d', created_at)"
	case "week":
		timeFormat = "strftime('%Y-W%W', created_at)"
	case "month":
		timeFormat = "strftime('%Y-%m', created_at)"
	default:
		return nil, fmt.Errorf("unsupported interval: %s", interval)
	}

	// Build the query with optional kind filter
	var query string
	var args []interface{}

	if kind != nil {
		// Filter by specific kind
		query = fmt.Sprintf(`
			SELECT %s as time_bucket, COUNT(*) as count
			FROM events
			WHERE kind = ?
			GROUP BY time_bucket
			ORDER BY MIN(created_at) DESC
			LIMIT ?
		`, timeFormat)
		args = append(args, *kind, limit)
	} else {
		// No kind filter
		query = fmt.Sprintf(`
			SELECT %s as time_bucket, COUNT(*) as count
			FROM events
			GROUP BY time_bucket
			ORDER BY MIN(created_at) DESC
			LIMIT ?
		`, timeFormat)
		args = append(args, limit)
	}

	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query event counts by time range: %w", err)
	}
	defer rows.Close()

	results := make(map[string]int)
	for rows.Next() {
		var timeBucket string
		var count int
		if err := rows.Scan(&timeBucket, &count); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		results[timeBucket] = count
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	return results, nil
}

type NostrEvent struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt time.Time  `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
}

type TimeFilter string

const (
	Last24Hours TimeFilter = "last_24_hours"
	LastWeek    TimeFilter = "last_week"
	LastMonth   TimeFilter = "last_month"
	AllTime     TimeFilter = "all_time"
)

// getTimeThreshold returns the time threshold for a given filter
func getTimeThreshold(filter TimeFilter) time.Time {
	now := time.Now()
	switch filter {
	case Last24Hours:
		return now.Add(-24 * time.Hour)
	case LastWeek:
		return now.Add(-7 * 24 * time.Hour)
	case LastMonth:
		return now.Add(-30 * 24 * time.Hour)
	default: // AllTime or any other value
		return time.Time{} // Zero time means no filter
	}
}

// getTimeFilterCondition returns the SQL condition for a time filter
func getTimeFilterCondition(filter TimeFilter) (string, []interface{}) {
	if filter == AllTime {
		return "1=1", nil
	}

	threshold := getTimeThreshold(filter)
	return "created_at >= ?", []any{threshold}
}

func (a *Analytics) RelayStats(ctx context.Context, filter TimeFilter) (map[string]interface{}, error) {
	timeCondition, args := getTimeFilterCondition(filter)

	// Build the query with placeholders
	query := fmt.Sprintf(`
		SELECT 
			relay_url, 
			last_timestamp,
			(SELECT COUNT(*) FROM events WHERE %[1]s) as total_events,
			(SELECT COUNT(DISTINCT pubkey) FROM events WHERE %[1]s) as unique_authors,
			(SELECT COUNT(DISTINCT kind) FROM events WHERE %[1]s) as unique_kinds
		FROM relay_state
	`, timeCondition)

	// Create a combined args array for all the subqueries
	queryArgs := []any{}
	if args != nil {
		// Add args for each subquery that uses the time condition
		queryArgs = append(queryArgs, args...)
		queryArgs = append(queryArgs, args...)
		queryArgs = append(queryArgs, args...)
	}

	rows, err := a.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to query relay stats: %w", err)
	}
	defer rows.Close()

	stats := make(map[string]interface{})
	relays := make(map[string]time.Time)

	var totalEvents, uniqueAuthors, uniqueKinds int

	for rows.Next() {
		var relayURL string
		var lastTimestamp time.Time

		if err := rows.Scan(&relayURL, &lastTimestamp, &totalEvents, &uniqueAuthors, &uniqueKinds); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}

		relays[relayURL] = lastTimestamp
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	stats["relays"] = relays
	stats["total_events"] = totalEvents
	stats["unique_authors"] = uniqueAuthors
	stats["unique_kinds"] = uniqueKinds
	stats["time_filter"] = string(filter)

	return stats, nil
}

func (a *Analytics) EventCountByKindWithTimeFilter(ctx context.Context, filter TimeFilter) (map[int]int, error) {
	timeCondition, args := getTimeFilterCondition(filter)
	query := fmt.Sprintf(`
		SELECT kind, COUNT(*) as count
		FROM events
		WHERE %s
		GROUP BY kind
		ORDER BY count DESC
	`, timeCondition)

	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query event counts by kind with filter %s: %w", filter, err)
	}
	defer rows.Close()

	results := make(map[int]int)
	for rows.Next() {
		var kind, count int
		if err := rows.Scan(&kind, &count); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		results[kind] = count
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	return results, nil
}
