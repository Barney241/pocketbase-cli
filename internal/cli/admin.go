package cli

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barney241/pocketbase-cli/internal/output"
	"github.com/Barney241/pocketbase-cli/internal/pb"
)

func (a *app) settingsCommand() *cobra.Command {
	settings := &cobra.Command{Use: "settings", Short: "Read and change the instance settings"}
	settings.AddCommand(a.settingsGetCommand(), a.settingsUpdateCommand(), a.settingsTestS3Command(), a.settingsTestEmailCommand(), a.settingsAppleSecretCommand())
	return settings
}

func (a *app) settingsGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "get [section]",
		Short:   "Show settings as dotted keys; secrets are masked by the server",
		Example: "  pbctl settings get\n  pbctl settings get smtp",
		RunE: func(cmd *cobra.Command, arguments []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			settings := map[string]any{}
			if err := client.JSON(cmd.Context(), http.MethodGet, "/api/settings", nil, nil, &settings); err != nil {
				return err
			}
			if len(arguments) > 0 {
				section, found := settings[arguments[0]]
				if !found {
					return usagef("no settings section %q (known: %s)", arguments[0], strings.Join(sortedKeys(settings), ", "))
				}
				settings = map[string]any{arguments[0]: section}
			}
			return a.printSettings(settings)
		},
	}
}

func (a *app) printSettings(settings map[string]any) error {
	if a.printer.Format != output.Table {
		return a.printer.Value(settings)
	}
	flat := map[string]any{}
	flatten("", settings, flat)
	for _, key := range sortedKeys(flat) {
		a.printer.Line("%s: %s", key, output.Cell(flat[key], a.printer.MaxCell))
	}
	return nil
}

func flatten(prefix string, node map[string]any, flat map[string]any) {
	for key, value := range node {
		dotted := key
		if prefix != "" {
			dotted = prefix + "." + key
		}
		if child, isObject := value.(map[string]any); isObject && len(child) > 0 {
			flatten(dotted, child, flat)
			continue
		}
		flat[dotted] = value
	}
}

func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (a *app) settingsUpdateCommand() *cobra.Command {
	var data string
	cmd := &cobra.Command{
		Use:     "update [dotted.key=value | dotted.key:=json ...]",
		Short:   "Change settings (needs --yes)",
		Example: "  pbctl settings update meta.appName='My App' logs.maxDays:=14 --yes",
		RunE: func(cmd *cobra.Command, arguments []string) error {
			changes, err := a.objectFrom(data, arguments)
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				return usagef("nothing to send: pass dotted.key=value pairs or --data")
			}
			if err := a.confirm("change the instance settings", true); err != nil {
				return err
			}
			client, err := a.pb()
			if err != nil {
				return err
			}
			settings := map[string]any{}
			if err := client.JSON(cmd.Context(), http.MethodPatch, "/api/settings", nil, changes, &settings); err != nil {
				return err
			}
			return a.printSettings(onlySections(settings, changes))
		},
	}
	cmd.Flags().StringVarP(&data, "data", "d", "", "JSON object, @file or - for stdin")
	return cmd
}

func onlySections(settings, changes map[string]any) map[string]any {
	touched := map[string]any{}
	for section := range changes {
		touched[section] = settings[section]
	}
	return touched
}

func (a *app) settingsTestS3Command() *cobra.Command {
	var filesystem string
	cmd := &cobra.Command{
		Use:   "test-s3",
		Short: "Check that the configured S3 storage works",
		Args:  exactArgs(0, "settings test-s3"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.postAndReport(cmd, "test the S3 "+filesystem+" filesystem", "/api/settings/test/s3", map[string]any{"filesystem": filesystem}, "S3 "+filesystem+" works")
		},
	}
	cmd.Flags().StringVar(&filesystem, "filesystem", "storage", "storage or backups")
	return cmd
}

func (a *app) settingsTestEmailCommand() *cobra.Command {
	var recipient, template, collection string
	cmd := &cobra.Command{
		Use:   "test-email --to <address>",
		Short: "Send a real test email",
		Args:  exactArgs(0, "settings test-email --to <address>"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			body := map[string]any{"email": recipient, "template": template, "collection": collection}
			return a.postAndReport(cmd, "send a test email to "+recipient, "/api/settings/test/email", body, "sent a "+template+" test email to "+recipient)
		},
	}
	cmd.Flags().StringVar(&recipient, "to", "", "recipient address")
	cmd.Flags().StringVar(&template, "template", "verification", "verification, password-reset, email-change, otp or login-alert")
	cmd.Flags().StringVar(&collection, "collection", "_superusers", "auth collection whose template is used")
	cmd.MarkFlagRequired("to")
	return cmd
}

func (a *app) settingsAppleSecretCommand() *cobra.Command {
	var data string
	cmd := &cobra.Command{
		Use:   "apple-client-secret [clientId=... teamId=... keyId=... privateKey=... duration:=15777000]",
		Short: "Generate a Sign in with Apple client secret",
		RunE: func(cmd *cobra.Command, arguments []string) error {
			body, err := a.objectFrom(data, arguments)
			if err != nil {
				return err
			}
			client, err := a.pb()
			if err != nil {
				return err
			}
			var secret any
			if err := client.JSON(cmd.Context(), http.MethodPost, "/api/settings/apple/generate-client-secret", nil, body, &secret); err != nil {
				return err
			}
			return a.printer.Value(secret)
		},
	}
	cmd.Flags().StringVarP(&data, "data", "d", "", "JSON object, @file or - for stdin")
	return cmd
}

func (a *app) postAndReport(cmd *cobra.Command, action, path string, body any, done string) error {
	if err := a.confirm(action, false); err != nil {
		return err
	}
	client, err := a.pb()
	if err != nil {
		return err
	}
	if err := client.JSON(cmd.Context(), http.MethodPost, path, nil, body, nil); err != nil {
		return err
	}
	a.printer.Line("%s", done)
	return nil
}

func (a *app) backupsCommand() *cobra.Command {
	backups := &cobra.Command{Use: "backups", Aliases: []string{"backup"}, Short: "List, create, download and restore backups"}
	backups.AddCommand(a.backupsListCommand(), a.backupsCreateCommand(), a.backupsDownloadCommand(), a.backupsUploadCommand(), a.backupsDeleteCommand(), a.backupsRestoreCommand())
	return backups
}

func (a *app) backupsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List backup files",
		Args:    exactArgs(0, "backups list"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			var backups []map[string]any
			if err := client.JSON(cmd.Context(), http.MethodGet, "/api/backups", nil, nil, &backups); err != nil {
				return err
			}
			if len(backups) == 0 {
				a.printer.Note("no backups")
			}
			return a.printer.List(backups, []string{"key", "size", "modified"}, backups)
		},
	}
}

func (a *app) backupsCreateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "create [name.zip]",
		Short: "Create a backup of the data directory",
		RunE: func(cmd *cobra.Command, arguments []string) error {
			name := ""
			if len(arguments) > 0 {
				name = arguments[0]
			}
			return a.postAndReport(cmd, "create a backup", "/api/backups", map[string]any{"name": name}, "backup "+firstNonEmpty(name, "(auto-named)")+" created")
		},
	}
}

func (a *app) backupsDownloadCommand() *cobra.Command {
	var destination string
	cmd := &cobra.Command{
		Use:   "download <key>",
		Short: "Save a backup archive to disk",
		Long:  "A backup holds the whole database, including the secrets that sign auth tokens.\nRead-only gateways and the pbguard server hook refuse this download for that reason.",
		Args:  exactArgs(1, "backups download <key>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			token, err := client.FileToken(cmd.Context())
			if err != nil {
				return err
			}
			request := pb.Request{Method: http.MethodGet, Path: "/api/backups/" + url.PathEscape(arguments[0]), Query: url.Values{"token": {token}}, Streaming: true, Anonymous: true}
			response, err := client.Do(cmd.Context(), request)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			return a.saveDownload(response, firstNonEmpty(destination, filepath.Base(arguments[0])))
		},
	}
	cmd.Flags().StringVar(&destination, "to", "", "where to save it")
	return cmd
}

func (a *app) backupsUploadCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "upload <file.zip>",
		Short: "Upload a backup archive",
		Args:  exactArgs(1, "backups upload <file.zip>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			if err := a.confirm("upload the backup "+arguments[0], false); err != nil {
				return err
			}
			client, err := a.pb()
			if err != nil {
				return err
			}
			body, contentType := streamUpload("file", arguments[0])
			response, err := client.Do(cmd.Context(), pb.Request{Method: http.MethodPost, Path: "/api/backups/upload", BodyStream: body, ContentType: contentType, Streaming: true})
			if err != nil {
				return err
			}
			response.Body.Close()
			a.printer.Line("uploaded %s", filepath.Base(arguments[0]))
			return nil
		},
	}
}

func streamUpload(field, location string) (io.Reader, string) {
	reader, pipe := io.Pipe()
	writer := multipart.NewWriter(pipe)
	go func() {
		err := attachFile(writer, field, location)
		if err == nil {
			err = writer.Close()
		}
		pipe.CloseWithError(err)
	}()
	return reader, writer.FormDataContentType()
}

func (a *app) backupsDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <key>",
		Short: "Delete a backup file (needs --yes)",
		Args:  exactArgs(1, "backups delete <key>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			return a.destroy(cmd.Context(), "delete the backup "+arguments[0], http.MethodDelete, "/api/backups/"+url.PathEscape(arguments[0]), "deleted backup "+arguments[0])
		},
	}
}

func (a *app) backupsRestoreCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <key>",
		Short: "Replace all current data with a backup and restart the server (needs --yes)",
		Args:  exactArgs(1, "backups restore <key>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			path := "/api/backups/" + url.PathEscape(arguments[0]) + "/restore"
			return a.destroy(cmd.Context(), "replace ALL current data with the backup "+arguments[0], http.MethodPost, path, "restore of "+arguments[0]+" started; the server restarts")
		},
	}
}

func (a *app) cronsCommand() *cobra.Command {
	crons := &cobra.Command{Use: "crons", Aliases: []string{"cron"}, Short: "List and trigger cron jobs"}
	crons.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List registered cron jobs",
			Args:    exactArgs(0, "crons list"),
			RunE: func(cmd *cobra.Command, _ []string) error {
				client, err := a.pb()
				if err != nil {
					return err
				}
				var jobs []map[string]any
				if err := client.JSON(cmd.Context(), http.MethodGet, "/api/crons", nil, nil, &jobs); err != nil {
					return err
				}
				return a.printer.List(jobs, []string{"id", "expression"}, jobs)
			},
		},
		&cobra.Command{
			Use:   "run <id>",
			Short: "Run a cron job now",
			Args:  exactArgs(1, "crons run <id>"),
			RunE: func(cmd *cobra.Command, arguments []string) error {
				return a.postAndReport(cmd, "run the cron job "+arguments[0], "/api/crons/"+url.PathEscape(arguments[0]), nil, "cron job "+arguments[0]+" triggered")
			},
		},
	)
	return crons
}

func (a *app) sqlCommand() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "sql [query]",
		Short: "Run raw SQL through the server's SQL endpoint (newer PocketBase versions)",
		Long:  "The endpoint can write, so read-only mode refuses it whatever the query is.\nFor a guaranteed read-only SELECT use `pbctl collections dry-run-view`.\nThe server returns at most 1000 rows.",
		RunE: func(cmd *cobra.Command, arguments []string) error {
			query, err := a.sqlQuery(file, arguments)
			if err != nil {
				return err
			}
			if err := a.confirm("run SQL: "+output.Truncate(query, 80), !strings.HasPrefix(strings.ToUpper(query), "SELECT")); err != nil {
				return err
			}
			client, err := a.pb()
			if err != nil {
				return err
			}
			var result struct {
				ExecTime     int64            `json:"execTime"`
				AffectedRows int64            `json:"affectedRows"`
				Columns      []map[string]any `json:"columns"`
				Rows         [][]any          `json:"rows"`
			}
			if err := client.JSON(cmd.Context(), http.MethodPost, "/api/sql", nil, map[string]string{"query": query}, &result); err != nil {
				return err
			}
			columns := []string{}
			for _, column := range result.Columns {
				columns = append(columns, fmt.Sprint(column["name"]))
			}
			rows := make([]map[string]any, 0, len(result.Rows))
			for _, values := range result.Rows {
				row := map[string]any{}
				for index, column := range columns {
					row[column] = values[index]
				}
				rows = append(rows, row)
			}
			if err := a.printer.List(result, columns, rows); err != nil {
				return err
			}
			a.printer.Note("%d row(s), %d affected, %d ms", len(rows), result.AffectedRows, result.ExecTime)
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "read the query from a file (- for stdin)")
	return cmd
}

func (a *app) sqlQuery(file string, arguments []string) (string, error) {
	switch {
	case file == "-":
		raw, err := io.ReadAll(a.stdin)
		return strings.TrimSpace(string(raw)), err
	case file != "":
		raw, err := os.ReadFile(file)
		return strings.TrimSpace(string(raw)), err
	case len(arguments) == 1:
		return strings.TrimSpace(arguments[0]), nil
	}
	return "", usagef("usage: pbctl sql \"<query>\" or pbctl sql --file <path>")
}
