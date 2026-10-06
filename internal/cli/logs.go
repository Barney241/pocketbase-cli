package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var logLevels = map[string]int{"debug": -4, "info": 0, "warn": 4, "error": 8}

var logLevelNames = map[string]string{"-4": "DEBUG", "0": "INFO", "4": "WARN", "8": "ERROR"}

var logDetailKeys = []string{"status", "execTime", "auth", "userIP", "error", "details"}

func (a *app) logsCommand() *cobra.Command {
	logs := &cobra.Command{Use: "logs", Aliases: []string{"log"}, Short: "Read the server logs"}
	logs.AddCommand(a.logsListCommand(), a.logsGetCommand(), a.logsStatsCommand(), a.logsTruncateCommand())
	return logs
}

type logFlags struct {
	level  string
	since  string
	status string
}

func (f *logFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.level, "level", "", "lowest level to show: debug, info, warn or error")
	cmd.Flags().StringVar(&f.since, "since", "", "only logs newer than this, e.g. 30m, 6h, 2d")
	cmd.Flags().StringVar(&f.status, "status", "", "only requests with this HTTP status, or a class such as 4xx or 5xx")
}

func (f *logFlags) conditions() ([]string, error) {
	conditions := []string{}
	if f.level != "" {
		level, known := logLevels[strings.ToLower(f.level)]
		if !known {
			return nil, usagef("unknown level %q (use debug, info, warn or error)", f.level)
		}
		conditions = append(conditions, fmt.Sprintf("level >= %d", level))
	}
	if f.since != "" {
		age, err := parseAge(f.since)
		if err != nil {
			return nil, err
		}
		conditions = append(conditions, fmt.Sprintf("created >= '%s'", pocketBaseTime(time.Now().Add(-age))))
	}
	if f.status != "" {
		condition, err := statusCondition(f.status)
		if err != nil {
			return nil, err
		}
		conditions = append(conditions, condition)
	}
	return conditions, nil
}

func statusCondition(status string) (string, error) {
	lowered := strings.ToLower(status)
	if len(lowered) == 3 && strings.HasSuffix(lowered, "xx") && lowered[0] >= '1' && lowered[0] <= '5' {
		class := int(lowered[0]-'0') * 100
		return fmt.Sprintf("(data.status >= %d && data.status < %d)", class, class+100), nil
	}
	var code int
	if _, err := fmt.Sscanf(status, "%d", &code); err != nil || code < 100 || code > 599 {
		return "", usagef("--status expects a code such as 404 or a class such as 5xx, got %q", status)
	}
	return fmt.Sprintf("data.status = %d", code), nil
}

func joinConditions(conditions []string, filter string) string {
	if filter != "" {
		conditions = append(conditions, "("+filter+")")
	}
	return strings.Join(conditions, " && ")
}

func (a *app) logsListCommand() *cobra.Command {
	flags := &listFlags{}
	shortcuts := &logFlags{}
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List logs, newest first, one line each",
		Example: "  pbctl logs list --level error --since 1h\n  pbctl logs list --status 5xx -f \"data.url ~ '/api/chat'\" -n 50",
		Args:    exactArgs(0, "logs list"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			conditions, err := shortcuts.conditions()
			if err != nil {
				return err
			}
			flags.filter = joinConditions(conditions, flags.filter)
			result, err := a.collect(cmd.Context(), client, "/api/logs", flags)
			if err != nil {
				return err
			}
			rows := make([]map[string]any, 0, len(result.Items))
			for _, entry := range result.Items {
				rows = append(rows, summarizeLog(entry))
			}
			return a.printListing(result, []string{"id", "created", "level", "message", "details"}, rows)
		},
	}
	flags.register(cmd, "-@rowid")
	shortcuts.register(cmd)
	return cmd
}

func summarizeLog(entry map[string]any) map[string]any {
	level := fmt.Sprint(entry["level"])
	data, _ := entry["data"].(map[string]any)
	details := []string{}
	for _, key := range logDetailKeys {
		if value, present := data[key]; present && !isZeroValue(value) {
			details = append(details, key+"="+logDetail(key, value))
		}
	}
	created, _, _ := strings.Cut(fmt.Sprint(entry["created"]), ".")
	return map[string]any{
		"id":      entry["id"],
		"created": created,
		"level":   firstNonEmpty(logLevelNames[level], level),
		"message": entry["message"],
		"details": strings.Join(details, " "),
	}
}

func logDetail(key string, value any) string {
	if number, isNumber := value.(json.Number); isNumber && key == "execTime" {
		if milliseconds, err := number.Float64(); err == nil {
			return fmt.Sprintf("%.1fms", milliseconds)
		}
	}
	return compactValue(value)
}

func compactValue(value any) string {
	if text, isText := value.(string); isText {
		return text
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func (a *app) logsGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Show one log entry with all its data",
		Args:  exactArgs(1, "logs get <id>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			entry := map[string]any{}
			if err := client.JSON(cmd.Context(), http.MethodGet, "/api/logs/"+url.PathEscape(arguments[0]), nil, nil, &entry); err != nil {
				return err
			}
			return a.printer.Object(entry)
		},
	}
}

func (a *app) logsStatsCommand() *cobra.Command {
	var filter string
	shortcuts := &logFlags{}
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Count logs per hour",
		Args:  exactArgs(0, "logs stats"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			conditions, err := shortcuts.conditions()
			if err != nil {
				return err
			}
			query := url.Values{}
			setIfPresent(query, "filter", joinConditions(conditions, filter))
			var buckets []map[string]any
			if err := client.JSON(cmd.Context(), http.MethodGet, "/api/logs/stats", query, nil, &buckets); err != nil {
				return err
			}
			if len(buckets) == 0 {
				a.printer.Note("no logs")
			}
			return a.printer.List(buckets, []string{"date", "total"}, buckets)
		},
	}
	cmd.Flags().StringVarP(&filter, "filter", "f", "", "PocketBase filter on level, message, created and data.<key>")
	shortcuts.register(cmd)
	return cmd
}

func (a *app) logsTruncateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "truncate",
		Short: "Delete all logs (needs --yes, PocketBase 0.40 or newer)",
		Args:  exactArgs(0, "logs truncate"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.destroy(cmd.Context(), "delete all logs", http.MethodDelete, "/api/logs", "deleted all logs")
		},
	}
}
