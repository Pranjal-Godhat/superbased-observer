package report

import (
	"database/sql"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type scheduleSpec struct {
	hour       int
	minute     int
	weekday    time.Weekday // -1 = every day (weekly mode, parts[4] == "*")
	dayOfMonth int          // only used when monthly = true
	monthly    bool
}

func StartScheduler(db *sql.DB, configPath string, logger *slog.Logger) {
	logger.Info("email report: scheduler started")

	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for tick := range ticker.C {
			cfg, err := loadEmailConfig(configPath)
			if err != nil {
				logger.Warn("email report: could not read config", "err", err)
				continue
			}
			if !cfg.Enabled || len(cfg.Recipients) == 0 {
				continue
			}

			spec := parseSchedule(cfg.Schedule)
			if shouldSend(tick, spec) {
				subject := "Weekly AI Report"
				if spec.monthly {
					subject = "Monthly AI Report"
				}
				logger.Info("email report: sending report", "type", subject)
				if err := SendReport(db, cfg); err != nil {
					logger.Error("email report: send failed", "err", err)
				} else {
					logger.Info("email report: sent successfully", "recipients", cfg.Recipients)
				}
			}
		}
	}()
}

func loadEmailConfig(configPath string) (Config, error) {
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Config{}, err
		}
		configPath = home + "/.observer/config.toml"
	}

	var raw struct {
		EmailReport struct {
			Enabled    bool     `toml:"enabled"`
			Schedule   string   `toml:"schedule"`
			Recipients []string `toml:"recipients"`
			Sections   []string `toml:"sections"`
			SMTP       struct {
				Host     string `toml:"host"`
				Port     int    `toml:"port"`
				Username string `toml:"username"`
				Password string `toml:"password"`
				From     string `toml:"from"`
			} `toml:"smtp"`
		} `toml:"email_report"`
	}

	body, err := os.ReadFile(configPath)
	if err != nil {
		return Config{}, err
	}
	if err := toml.Unmarshal(body, &raw); err != nil {
		return Config{}, err
	}

	e := raw.EmailReport
	return Config{
		Enabled:    e.Enabled,
		Schedule:   e.Schedule,
		Recipients: e.Recipients,
		Sections:   e.Sections,
		SMTPHost:   e.SMTP.Host,
		SMTPPort:   e.SMTP.Port,
		Username:   e.SMTP.Username,
		Password:   e.SMTP.Password,
		From:       e.SMTP.From,
	}, nil
}

// parseSchedule parses a 5-field cron string.
// Monthly mode: parts[2] != "*"  e.g. "30 10 15 * *" → day 15 at 10:30
// Weekly mode:  parts[2] == "*"  e.g. "0 9 * * MON"  → every Monday at 9:00
func parseSchedule(cron string) scheduleSpec {
	spec := scheduleSpec{hour: 9, minute: 0, weekday: time.Monday}
	parts := strings.Fields(cron)
	if len(parts) != 5 {
		return spec
	}
	if m, err := strconv.Atoi(parts[0]); err == nil {
		spec.minute = m
	}
	if h, err := strconv.Atoi(parts[1]); err == nil {
		spec.hour = h
	}
	if parts[2] != "*" {
		// Monthly mode
		if dom, err := strconv.Atoi(parts[2]); err == nil {
			spec.monthly = true
			spec.dayOfMonth = dom
		}
	} else {
		// Weekly mode — parse weekday
		switch strings.ToUpper(parts[4]) {
		case "0", "SUN":
			spec.weekday = time.Sunday
		case "1", "MON":
			spec.weekday = time.Monday
		case "2", "TUE":
			spec.weekday = time.Tuesday
		case "3", "WED":
			spec.weekday = time.Wednesday
		case "4", "THU":
			spec.weekday = time.Thursday
		case "5", "FRI":
			spec.weekday = time.Friday
		case "6", "SAT":
			spec.weekday = time.Saturday
		case "*":
			spec.weekday = -1 // every day
		}
	}
	return spec
}

func shouldSend(t time.Time, spec scheduleSpec) bool {
	if t.Hour() != spec.hour || t.Minute() != spec.minute {
		return false
	}
	if spec.monthly {
		return t.Day() == spec.dayOfMonth
	}
	if spec.weekday == -1 {
		return true // every day
	}
	return t.Weekday() == spec.weekday
}
