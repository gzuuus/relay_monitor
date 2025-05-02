package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type MCPServer struct {
	analytics *Analytics
	server    *server.MCPServer
}

func NewMCPServer(analytics *Analytics) *MCPServer {
	s := server.NewMCPServer(
		"Nostr Relay Analytics",
		"1.0.0",
		server.WithToolCapabilities(true),
		server.WithLogging(),
		server.WithRecovery(),
	)

	mcpServer := &MCPServer{
		analytics: analytics,
		server:    s,
	}

	mcpServer.registerTools()
	return mcpServer
}

func (m *MCPServer) StartStdio() error {
	return server.ServeStdio(m.server)
}

func (m *MCPServer) registerTools() {
	m.server.AddTool(
		mcp.NewTool("event_count_by_kind",
			mcp.WithDescription("Get count of Nostr events grouped by kind with time filter"),
			mcp.WithString("time_filter",
				mcp.Description("Time filter to apply (last_24_hours, last_week, last_month, all_time)"),
				mcp.Enum("last_24_hours", "last_week", "last_month", "all_time"),
			),
		),
		m.handleEventCountByKind,
	)

	m.server.AddTool(
		mcp.NewTool("top_authors",
			mcp.WithDescription("Get the most active authors (pubkeys) by event count with optional kind filtering"),
			mcp.WithNumber("kind",
				mcp.Description("Optional event kind to filter by"),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum number of authors to return"),
			),
			mcp.WithString("time_filter",
				mcp.Description("Time filter to apply (last_24_hours, last_week, last_month, all_time)"),
				mcp.Enum("last_24_hours", "last_week", "last_month", "all_time"),
			),
		),
		m.handleTopAuthors,
	)

	m.server.AddTool(
		mcp.NewTool("event_count_by_time",
			mcp.WithDescription("Get event counts aggregated by time periods with optional kind filtering"),
			mcp.WithString("interval",
				mcp.Required(),
				mcp.Description("Time interval for aggregation (hour, day, week, month)"),
				mcp.Enum("hour", "day", "week", "month"),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum number of time periods to return"),
			),
			mcp.WithNumber("kind",
				mcp.Description("Optional event kind to filter by"),
			),
		),
		m.handleEventCountByTime,
	)

	m.server.AddTool(
		mcp.NewTool("relay_stats",
			mcp.WithDescription("Get statistics about the relay monitoring"),
			mcp.WithString("time_filter",
				mcp.Description("Time filter to apply (last_24_hours, last_week, last_month, all_time)"),
				mcp.Enum("last_24_hours", "last_week", "last_month", "all_time"),
			),
		),
		m.handleRelayStats,
	)
}

func jsonResult(data any) (*mcp.CallToolResult, error) {
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("error serializing results: %v", err)), nil
	}
	return mcp.NewToolResultText(string(jsonData)), nil
}

func getTimeFilter(request mcp.CallToolRequest) TimeFilter {
	timeFilterStr, ok := getStringParam(request, "time_filter")
	if !ok || timeFilterStr == "" {
		return AllTime
	}

	return TimeFilter(timeFilterStr)
}

func (m *MCPServer) handleEventCountByKind(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	timeFilter := getTimeFilter(request)
	
	counts, err := m.analytics.EventCountByKindWithTimeFilter(ctx, timeFilter)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("error getting event counts: %v", err)), nil
	}

	var results []map[string]any
	for kind, count := range counts {
		results = append(results, map[string]any{
			"kind":  kind,
			"count": count,
		})
	}

	return jsonResult(results)
}

func getIntParam(request mcp.CallToolRequest, paramName string, defaultValue int) int {
	if paramValue, ok := request.Params.Arguments[paramName]; ok {
		if floatValue, ok := paramValue.(float64); ok {
			return int(floatValue)
		}
	}
	return defaultValue
}

func (m *MCPServer) handleTopAuthors(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := getIntParam(request, "limit", 10)
	timeFilter := getTimeFilter(request)
	kindPtr := getOptionalIntParam(request, "kind")
	
	var authors []AuthorStats
	var err error
	
	if kindPtr != nil {
		authors, err = m.analytics.TopAuthorsByKind(ctx, *kindPtr, limit, timeFilter)
	} else {
		authors, err = m.analytics.TopAuthors(ctx, limit, timeFilter)
	}
	
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("error getting top authors: %v", err)), nil
	}

	return jsonResult(authors)
}

func getStringParam(request mcp.CallToolRequest, paramName string) (string, bool) {
	if paramValue, ok := request.Params.Arguments[paramName]; ok {
		if stringValue, ok := paramValue.(string); ok {
			return stringValue, true
		}
	}
	return "", false
}

func getOptionalIntParam(request mcp.CallToolRequest, paramName string) *int {
	if paramValue, ok := request.Params.Arguments[paramName]; ok {
		if floatValue, ok := paramValue.(float64); ok {
			intVal := int(floatValue)
			return &intVal
		}
	}
	return nil
}

func (m *MCPServer) handleEventCountByTime(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	interval, ok := getStringParam(request, "interval")
	if !ok {
		return mcp.NewToolResultError("missing or invalid interval parameter"), nil
	}

	limit := getIntParam(request, "limit", 20)
	kindPtr := getOptionalIntParam(request, "kind")

	counts, err := m.analytics.EventCountByTimeRange(ctx, interval, limit, kindPtr)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("error getting event counts by time: %v", err)), nil
	}

	return jsonResult(counts)
}

func (m *MCPServer) handleRelayStats(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	timeFilter := getTimeFilter(request)

	stats, err := m.analytics.RelayStats(ctx, timeFilter)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("error getting relay stats: %v", err)), nil
	}

	return jsonResult(stats)
}
