package main

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

var dbAccessMutex sync.Mutex

type Analytics struct {
	db *sql.DB
}

func NewAnalytics(db *sql.DB) *Analytics {
	return &Analytics{db: db}
}

func (a *Analytics) EventCountByKind(ctx context.Context) ([]KindStats, error) {
	return a.EventCountByKindWithTimeFilter(ctx, AllTime)
}

type AuthorStats struct {
	PubKey string `json:"pubkey"`
	Count  int    `json:"count"`
}

type TimeStats struct {
	Time  string `json:"time"`
	Count int    `json:"count"`
}

type KindStats struct {
	Kind  int `json:"kind"`
	Count int `json:"count"`
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

	queryArgs := []any{}
	if args != nil {
		queryArgs = append(queryArgs, args...)
	}
	queryArgs = append(queryArgs, limit)

	dbAccessMutex.Lock()
	defer dbAccessMutex.Unlock()
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

	queryArgs := []any{kind}
	if args != nil {
		queryArgs = append(queryArgs, args...)
	}
	queryArgs = append(queryArgs, limit)

	dbAccessMutex.Lock()
	defer dbAccessMutex.Unlock()
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

func (a *Analytics) EventCountByTimeRange(ctx context.Context, interval string, limit int, kind *int) ([]TimeStats, error) {
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

	var query string
	var args []any

	if kind != nil {
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
		query = fmt.Sprintf(`
			SELECT %s as time_bucket, COUNT(*) as count
			FROM events
			GROUP BY time_bucket
			ORDER BY MIN(created_at) DESC
			LIMIT ?
		`, timeFormat)
		args = append(args, limit)
	}

	dbAccessMutex.Lock()
	defer dbAccessMutex.Unlock()
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query event counts by time range: %w", err)
	}
	defer rows.Close()

	var results []TimeStats
	for rows.Next() {
		var timeBucket string
		var count int
		if err := rows.Scan(&timeBucket, &count); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		results = append(results, TimeStats{
			Time:  timeBucket,
			Count: count,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	return results, nil
}

type TimeFilter string

const (
	Last24Hours TimeFilter = "last_24_hours"
	LastWeek    TimeFilter = "last_week"
	LastMonth   TimeFilter = "last_month"
	AllTime     TimeFilter = "all_time"
)

func getTimeThreshold(filter TimeFilter) time.Time {
	now := time.Now()
	switch filter {
	case Last24Hours:
		return now.Add(-24 * time.Hour)
	case LastWeek:
		return now.Add(-7 * 24 * time.Hour)
	case LastMonth:
		return now.Add(-30 * 24 * time.Hour)
	default:
		return time.Time{}
	}
}

func getTimeFilterCondition(filter TimeFilter) (string, []any) {
	if filter == AllTime {
		return "1=1", nil
	}

	threshold := getTimeThreshold(filter)
	return "created_at >= ?", []any{threshold}
}

func (a *Analytics) RelayStats(ctx context.Context, filter TimeFilter) (map[string]any, error) {
	timeCondition, args := getTimeFilterCondition(filter)


	overallStatsQuery := fmt.Sprintf(`
		SELECT
			COUNT(*) as total_events,
			COUNT(DISTINCT pubkey) as unique_authors,
			COUNT(DISTINCT kind) as unique_kinds
		FROM events
		WHERE %s
	`, timeCondition)

	dbAccessMutex.Lock()
	var totalEvents, uniqueAuthors, uniqueKinds int
	err := a.db.QueryRowContext(ctx, overallStatsQuery, args...).Scan(&totalEvents, &uniqueAuthors, &uniqueKinds)
	dbAccessMutex.Unlock()
	if err != nil {
		return nil, fmt.Errorf("failed to query overall stats: %w", err)
	}


	relayStatsQuery := fmt.Sprintf(`
		SELECT
			relay_url,
			COUNT(*) as event_count,
			COUNT(DISTINCT pubkey) as author_count,
			COUNT(DISTINCT kind) as kind_count
		FROM events
		WHERE %s AND relay_url IS NOT NULL
		GROUP BY relay_url
		ORDER BY event_count DESC
	`, timeCondition)

	dbAccessMutex.Lock() // Lock for per-relay stats query
	relayStatsRows, err := a.db.QueryContext(ctx, relayStatsQuery, args...)
	dbAccessMutex.Unlock()
	if err != nil {
		return nil, fmt.Errorf("failed to query per-relay stats: %w", err)
	}
	defer relayStatsRows.Close()

	relayDetails := make(map[string]map[string]any)
	var activeRelays []string

	for relayStatsRows.Next() {
		var relayURL string
		var eventCount, authorCount, kindCount int
		if err := relayStatsRows.Scan(&relayURL, &eventCount, &authorCount, &kindCount); err != nil {
			return nil, fmt.Errorf("failed to scan relay stats row: %w", err)
		}
		relayDetails[relayURL] = map[string]any{
			"event_count":  eventCount,
			"author_count": authorCount,
			"kind_count":   kindCount,
		}
		activeRelays = append(activeRelays, relayURL)
	}

	stats := make(map[string]any)
	stats["total_events"] = totalEvents
	stats["unique_authors"] = uniqueAuthors
	stats["unique_kinds"] = uniqueKinds
	stats["time_filter"] = string(filter)
	stats["active_relays"] = activeRelays
	stats["relay_details"] = relayDetails

	return stats, nil
}

func (a *Analytics) EventCountByKindWithTimeFilter(ctx context.Context, filter TimeFilter) ([]KindStats, error) {
	timeCondition, args := getTimeFilterCondition(filter)
	query := fmt.Sprintf(`
		SELECT kind, COUNT(*) as count
		FROM events
		WHERE %s
		GROUP BY kind
		ORDER BY count DESC
	`, timeCondition)

	dbAccessMutex.Lock()
	defer dbAccessMutex.Unlock()
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query event counts by kind with filter %s: %w", filter, err)
	}
	defer rows.Close()

	var results []KindStats
	for rows.Next() {
		var kind, count int
		if err := rows.Scan(&kind, &count); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		results = append(results, KindStats{
			Kind:  kind,
			Count: count,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	return results, nil
}
