package report

import (
	"bytes"
	"database/sql"
	"fmt"
	"html/template"
	"net/smtp"
	"sort"
	"strings"
	"time"
)

type Config struct {
	Enabled    bool
	Schedule   string
	Recipients []string
	Sections   []string
	SMTPHost   string
	SMTPPort   int
	Username   string
	Password   string
	From       string
}

type CostSummary struct {
	TotalCost   float64
	TotalTurns  int64
	TotalInput  int64
	TotalOutput int64
}

type SessionStats struct {
	TotalSessions   int
	TotalActions    int64
	AvgQualityScore float64
	AvgErrorRate    float64
	AvgRedundancy   float64
}

type CacheStats struct {
	CacheReadTokens     int64
	CacheCreationTokens int64
}

type PerformanceStats struct {
	AvgTimeToFirstTokenMs float64
	AvgResponseMs         float64
	TotalToolUseCount     int64
}

type SessionRow struct {
	ID    string
	Tool  string
	Cost  float64
	Turns int64
}

type ModelRow struct {
	Model string
	Cost  float64
	Turns int64
}

type WeeklyData struct {
	Period string

	HasCost        bool
	Cost           CostSummary
	HasSessions    bool
	Sessions       SessionStats
	HasCache       bool
	Cache          CacheStats
	HasPerformance bool
	Performance    PerformanceStats
	HasTopSessions bool
	TopSessions    []SessionRow
	HasByModel     bool
	ByModel        []ModelRow
}

func sectionEnabled(sections []string, name string) bool {
	for _, s := range sections {
		if s == name {
			return true
		}
	}
	return false
}

func combinedCTE() string {
	return `
WITH proxy_turn_ids AS (
	SELECT request_id FROM api_turns
	WHERE request_id IS NOT NULL AND request_id != ''
	  AND timestamp >= ? AND timestamp < ?
),
combined AS (
	SELECT
		session_id,
		model,
		COALESCE(input_tokens, 0)  AS input_tokens,
		COALESCE(output_tokens, 0) AS output_tokens,
		COALESCE(cache_read_tokens, 0) AS cache_read_tokens,
		COALESCE(cache_creation_tokens, 0) + COALESCE(cache_creation_1h_tokens, 0) AS cache_creation_tokens,
		COALESCE(cost_usd, 0) AS cost_usd,
		COALESCE(time_to_first_token_ms, 0) AS time_to_first_token_ms,
		COALESCE(total_response_ms, 0)       AS total_response_ms,
		COALESCE(tool_use_count, 0)          AS tool_use_count
	FROM api_turns
	WHERE timestamp >= ? AND timestamp < ?
	  AND (error_class IS NULL OR error_class = '')
	UNION ALL
	SELECT
		session_id,
		model,
		COALESCE(input_tokens, 0),
		COALESCE(output_tokens, 0),
		COALESCE(cache_read_tokens, 0),
		COALESCE(cache_creation_tokens, 0),
		COALESCE(estimated_cost_usd, 0),
		0, 0, 0
	FROM token_usage
	WHERE timestamp >= ? AND timestamp < ?
	  AND (source_event_id IS NULL OR source_event_id = ''
	       OR source_event_id NOT IN (SELECT request_id FROM proxy_turn_ids))
)`
}

func combinedArgs(since, until string) []any {
	return []any{since, until, since, until, since, until}
}

// deriveWindow returns since/until based on the schedule.
// Monthly → previous full calendar month.
// Weekly/other → last 7 days.
func deriveWindow(schedule string) (since, until time.Time) {
	spec := parseSchedule(schedule)
	now := time.Now().UTC()
	if spec.monthly {
		firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		until = firstOfThisMonth
		since = firstOfThisMonth.AddDate(0, -1, 0)
	} else {
		until = now
		since = now.AddDate(0, 0, -7)
	}
	return
}

func BuildReport(db *sql.DB, sections []string, schedule string) (*WeeklyData, error) {
	since, until := deriveWindow(schedule)
	sinceStr := since.Format(time.RFC3339)
	untilStr := until.Format(time.RFC3339)

	data := &WeeklyData{
		Period: since.Format("Jan 2") + " - " + until.Format("Jan 2, 2006"),
	}

	if sectionEnabled(sections, "cost") {
		data.HasCost = true
		args := combinedArgs(sinceStr, untilStr)
		row := db.QueryRow(combinedCTE()+`
			SELECT
				COALESCE(SUM(cost_usd), 0),
				COUNT(*),
				COALESCE(SUM(input_tokens), 0),
				COALESCE(SUM(output_tokens), 0)
			FROM combined`, args...)
		if err := row.Scan(&data.Cost.TotalCost, &data.Cost.TotalTurns,
			&data.Cost.TotalInput, &data.Cost.TotalOutput); err != nil {
			return nil, fmt.Errorf("cost summary: %w", err)
		}
	}

	if sectionEnabled(sections, "sessions") {
		data.HasSessions = true
		row := db.QueryRow(`
			SELECT
				COUNT(*),
				COALESCE(SUM(total_actions), 0),
				COALESCE(AVG(quality_score), 0),
				COALESCE(AVG(error_rate), 0),
				COALESCE(AVG(redundancy_ratio), 0)
			FROM sessions
			WHERE started_at >= ? AND started_at < ?`,
			sinceStr, untilStr,
		)
		if err := row.Scan(&data.Sessions.TotalSessions, &data.Sessions.TotalActions,
			&data.Sessions.AvgQualityScore, &data.Sessions.AvgErrorRate,
			&data.Sessions.AvgRedundancy); err != nil {
			return nil, fmt.Errorf("session stats: %w", err)
		}
	}

	if sectionEnabled(sections, "cache") {
		data.HasCache = true
		args := combinedArgs(sinceStr, untilStr)
		row := db.QueryRow(combinedCTE()+`
			SELECT
				COALESCE(SUM(cache_read_tokens), 0),
				COALESCE(SUM(cache_creation_tokens), 0)
			FROM combined`, args...)
		if err := row.Scan(&data.Cache.CacheReadTokens, &data.Cache.CacheCreationTokens); err != nil {
			return nil, fmt.Errorf("cache stats: %w", err)
		}
	}

	if sectionEnabled(sections, "performance") {
		data.HasPerformance = true
		args := combinedArgs(sinceStr, untilStr)
		row := db.QueryRow(combinedCTE()+`
			SELECT
				COALESCE(AVG(NULLIF(time_to_first_token_ms, 0)), 0),
				COALESCE(AVG(NULLIF(total_response_ms, 0)), 0),
				COALESCE(SUM(tool_use_count), 0)
			FROM combined
			WHERE time_to_first_token_ms > 0
			   OR total_response_ms > 0
			   OR tool_use_count > 0`, args...)
		if err := row.Scan(&data.Performance.AvgTimeToFirstTokenMs,
			&data.Performance.AvgResponseMs, &data.Performance.TotalToolUseCount); err != nil {
			return nil, fmt.Errorf("performance stats: %w", err)
		}
	}

	if sectionEnabled(sections, "top_sessions") {
		data.HasTopSessions = true
		args := combinedArgs(sinceStr, untilStr)
		rows, err := db.Query(combinedCTE()+`
			SELECT
				COALESCE(c.session_id, ''),
				COALESCE(s.tool, ''),
				COALESCE(SUM(c.cost_usd), 0),
				COUNT(*)
			FROM combined c
			LEFT JOIN sessions s ON s.id = c.session_id
			GROUP BY c.session_id, s.tool
			ORDER BY SUM(c.cost_usd) DESC
			LIMIT 5`, args...)
		if err != nil {
			return nil, fmt.Errorf("top sessions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r SessionRow
			if err := rows.Scan(&r.ID, &r.Tool, &r.Cost, &r.Turns); err == nil {
				data.TopSessions = append(data.TopSessions, r)
			}
		}
	}

	if sectionEnabled(sections, "by_model") {
		data.HasByModel = true
		args := combinedArgs(sinceStr, untilStr)
		rows, err := db.Query(combinedCTE()+`
			SELECT
				COALESCE(model, '(unattributed)'),
				COALESCE(SUM(cost_usd), 0),
				COUNT(*)
			FROM combined
			GROUP BY model
			ORDER BY SUM(cost_usd) DESC`, args...)
		if err != nil {
			return nil, fmt.Errorf("by model: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r ModelRow
			if err := rows.Scan(&r.Model, &r.Cost, &r.Turns); err == nil {
				data.ByModel = append(data.ByModel, r)
			}
		}
		sort.Slice(data.ByModel, func(i, j int) bool { return data.ByModel[i].Cost > data.ByModel[j].Cost })
	}

	return data, nil
}

func mulf(a, b float64) float64 { return a * b }

var emailTmpl = template.Must(template.New("report").Funcs(template.FuncMap{"mulf": mulf}).Parse(`<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;max-width:640px;margin:auto;color:#333;padding:20px">
  <h2 style="color:#1a1a1a">{{.Title}}</h2>
  <p style="color:#666">Period: <strong>{{.Period}}</strong></p>

  {{if .HasCost}}
  <h3 style="margin-top:28px">Cost summary</h3>
  <table width="100%" cellpadding="10" style="border-collapse:collapse">
    <tr style="background:#f5f5f5"><td style="border:1px solid #ddd"><strong>Total Cost</strong></td><td style="border:1px solid #ddd">${{printf "%.4f" .Cost.TotalCost}}</td></tr>
    <tr><td style="border:1px solid #ddd"><strong>Total Turns</strong></td><td style="border:1px solid #ddd">{{.Cost.TotalTurns}}</td></tr>
    <tr style="background:#f5f5f5"><td style="border:1px solid #ddd"><strong>Input Tokens</strong></td><td style="border:1px solid #ddd">{{.Cost.TotalInput}}</td></tr>
    <tr><td style="border:1px solid #ddd"><strong>Output Tokens</strong></td><td style="border:1px solid #ddd">{{.Cost.TotalOutput}}</td></tr>
  </table>
  {{end}}

  {{if .HasSessions}}
  <h3 style="margin-top:28px">Session stats</h3>
  <table width="100%" cellpadding="10" style="border-collapse:collapse">
    <tr style="background:#f5f5f5"><td style="border:1px solid #ddd"><strong>Sessions</strong></td><td style="border:1px solid #ddd">{{.Sessions.TotalSessions}}</td></tr>
    <tr><td style="border:1px solid #ddd"><strong>Total Actions</strong></td><td style="border:1px solid #ddd">{{.Sessions.TotalActions}}</td></tr>
    <tr style="background:#f5f5f5"><td style="border:1px solid #ddd"><strong>Avg Quality Score</strong></td><td style="border:1px solid #ddd">{{printf "%.2f" .Sessions.AvgQualityScore}}</td></tr>
    <tr><td style="border:1px solid #ddd"><strong>Avg Error Rate</strong></td><td style="border:1px solid #ddd">{{printf "%.2f%%" (mulf .Sessions.AvgErrorRate 100)}}</td></tr>
  </table>
  {{end}}

  {{if .HasCache}}
  <h3 style="margin-top:28px">Cache &amp; savings</h3>
  <table width="100%" cellpadding="10" style="border-collapse:collapse">
    <tr style="background:#f5f5f5"><td style="border:1px solid #ddd"><strong>Cache Read Tokens</strong></td><td style="border:1px solid #ddd">{{.Cache.CacheReadTokens}}</td></tr>
    <tr><td style="border:1px solid #ddd"><strong>Cache Write Tokens</strong></td><td style="border:1px solid #ddd">{{.Cache.CacheCreationTokens}}</td></tr>
  </table>
  {{end}}

  {{if .HasPerformance}}
  <h3 style="margin-top:28px">Performance</h3>
  <table width="100%" cellpadding="10" style="border-collapse:collapse">
    <tr style="background:#f5f5f5"><td style="border:1px solid #ddd"><strong>Avg Time to First Token</strong></td><td style="border:1px solid #ddd">{{printf "%.0f" .Performance.AvgTimeToFirstTokenMs}} ms</td></tr>
    <tr><td style="border:1px solid #ddd"><strong>Avg Response Time</strong></td><td style="border:1px solid #ddd">{{printf "%.0f" .Performance.AvgResponseMs}} ms</td></tr>
    <tr style="background:#f5f5f5"><td style="border:1px solid #ddd"><strong>Total Tool Calls</strong></td><td style="border:1px solid #ddd">{{.Performance.TotalToolUseCount}}</td></tr>
  </table>
  {{end}}

  {{if .HasTopSessions}}
  <h3 style="margin-top:28px">Top sessions by cost</h3>
  <table width="100%" cellpadding="10" style="border-collapse:collapse">
    <tr style="background:#f5f5f5"><th align="left" style="border:1px solid #ddd">Session</th><th align="left" style="border:1px solid #ddd">Tool</th><th align="right" style="border:1px solid #ddd">Cost</th><th align="right" style="border:1px solid #ddd">Turns</th></tr>
    {{range .TopSessions}}
    <tr><td style="border:1px solid #ddd;font-family:monospace;font-size:11px">{{.ID}}</td><td style="border:1px solid #ddd">{{.Tool}}</td><td align="right" style="border:1px solid #ddd">${{printf "%.4f" .Cost}}</td><td align="right" style="border:1px solid #ddd">{{.Turns}}</td></tr>
    {{end}}
  </table>
  {{end}}

  {{if .HasByModel}}
  <h3 style="margin-top:28px">Cost by model</h3>
  <table width="100%" cellpadding="10" style="border-collapse:collapse">
    <tr style="background:#f5f5f5"><th align="left" style="border:1px solid #ddd">Model</th><th align="right" style="border:1px solid #ddd">Cost</th><th align="right" style="border:1px solid #ddd">Turns</th></tr>
    {{range .ByModel}}
    <tr><td style="border:1px solid #ddd">{{.Model}}</td><td align="right" style="border:1px solid #ddd">${{printf "%.4f" .Cost}}</td><td align="right" style="border:1px solid #ddd">{{.Turns}}</td></tr>
    {{end}}
  </table>
  {{end}}

  <p style="color:#999;font-size:12px;margin-top:32px">
    Sent by superbased-observer · <a href="http://localhost:8081">Open dashboard</a>
  </p>
</body>
</html>`))

func SendReport(db *sql.DB, cfg Config) error {
	sections := cfg.Sections
	if len(sections) == 0 {
		sections = []string{"cost", "sessions", "top_sessions"}
	}

	spec := parseSchedule(cfg.Schedule)
	subject := "Weekly AI Report"
	if spec.monthly {
		subject = "Monthly AI Report"
	}

	data, err := BuildReport(db, sections, cfg.Schedule)
	if err != nil {
		return fmt.Errorf("build report: %w", err)
	}

	// Add title to template data
	type reportData struct {
		Title string
		*WeeklyData
	}
	td := reportData{Title: subject, WeeklyData: data}

	var buf bytes.Buffer
	if err := emailTmpl.Execute(&buf, td); err != nil {
		return fmt.Errorf("render template: %w", err)
	}
	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.SMTPHost)
	msg := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s - %s\r\n"+
			"MIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s",
		cfg.From,
		strings.Join(cfg.Recipients, ", "),
		subject,
		data.Period,
		buf.String(),
	)
	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	return smtp.SendMail(addr, auth, cfg.From, cfg.Recipients, []byte(msg))
}
