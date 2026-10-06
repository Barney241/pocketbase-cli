package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barney241/pocketbase-cli/internal/pb"
)

const (
	importBatchSize       = 50
	unattendedWatchWindow = time.Minute
	largestRealtimeEvent  = 8 << 20
)

type writeFlags struct {
	data    string
	uploads []string
	expand  string
	fields  string
}

func (f *writeFlags) register(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.StringVarP(&f.data, "data", "d", "", "JSON object, @file or - for stdin")
	flags.StringArrayVar(&f.uploads, "file", nil, "upload a file into a file field: field=path (repeatable)")
	flags.StringVar(&f.expand, "expand", "", "relations to expand in the returned record")
	flags.StringVar(&f.fields, "fields", "", "only return these fields")
}

func (a *app) recordsCommand() *cobra.Command {
	records := &cobra.Command{Use: "records", Aliases: []string{"record", "r"}, Short: "Read and write records"}
	records.AddCommand(
		a.recordsListCommand(), a.recordsGetCommand(), a.recordsCountCommand(), a.recordsWatchCommand(),
		a.recordsCreateCommand(), a.recordsUpdateCommand(), a.recordsDeleteCommand(),
		a.recordsImportCommand(), a.recordsBatchCommand(),
	)
	return records
}

func (a *app) recordsListCommand() *cobra.Command {
	flags := &listFlags{}
	cmd := &cobra.Command{
		Use:     "list <collection>",
		Aliases: []string{"ls"},
		Short:   "List records, 20 per page",
		Example: "  pbctl records list posts -f \"status = 'open'\" -s -created --fields id,title\n  pbctl records list users -f 'email = {:e}' --param e=\"o'brien@x.io\"\n  pbctl records list posts --all -o jsonl > posts.jsonl",
		Args:    exactArgs(1, "records list <collection>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			result, err := a.collect(cmd.Context(), client, collectionPath(arguments[0], "records"), flags)
			if err != nil {
				return err
			}
			return a.printListing(result, requestedColumns(flags.fields), result.Items)
		},
	}
	flags.register(cmd, "")
	return cmd
}

func (a *app) recordsGetCommand() *cobra.Command {
	var expand, fields string
	cmd := &cobra.Command{
		Use:   "get <collection> <id>",
		Short: "Show one record in full",
		Args:  exactArgs(2, "records get <collection> <id>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			query := url.Values{}
			setIfPresent(query, "expand", expand)
			setIfPresent(query, "fields", fields)
			record := map[string]any{}
			if err := client.JSON(cmd.Context(), http.MethodGet, collectionPath(arguments[0], "records", arguments[1]), query, nil, &record); err != nil {
				return err
			}
			return a.printer.Object(record)
		},
	}
	cmd.Flags().StringVar(&expand, "expand", "", "relations to expand")
	cmd.Flags().StringVar(&fields, "fields", "", "only return these fields")
	return cmd
}

func (a *app) recordsCountCommand() *cobra.Command {
	var filter string
	var parameters []string
	cmd := &cobra.Command{
		Use:   "count <collection>",
		Short: "Count records, optionally matching a filter",
		Args:  exactArgs(1, "records count <collection>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			bound, err := bindFilter(filter, parameters)
			if err != nil {
				return err
			}
			query := url.Values{"perPage": {"1"}, "fields": {"id"}}
			setIfPresent(query, "filter", bound)
			page := &listing{}
			if err := client.JSON(cmd.Context(), http.MethodGet, collectionPath(arguments[0], "records"), query, nil, page); err != nil {
				return err
			}
			a.printer.Line("%d", page.TotalItems)
			return nil
		},
	}
	cmd.Flags().StringVarP(&filter, "filter", "f", "", "PocketBase filter")
	cmd.Flags().StringArrayVar(&parameters, "param", nil, "value for a {:name} placeholder in --filter")
	return cmd
}

func (a *app) recordsCreateCommand() *cobra.Command {
	flags := &writeFlags{}
	cmd := &cobra.Command{
		Use:     "create <collection> [field=value | field:=json ...]",
		Short:   "Create a record",
		Example: "  pbctl records create posts title='Hello' views:=0 tags:='[\"a\",\"b\"]'\n  pbctl records create docs -d @doc.json --file attachment=./plan.pdf",
		Args:    minimumArgs(1, "records create <collection> [field=value ...]"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			if err := a.confirm("create a record in "+arguments[0], false); err != nil {
				return err
			}
			return a.writeRecord(cmd.Context(), http.MethodPost, collectionPath(arguments[0], "records"), flags, arguments[1:])
		},
	}
	flags.register(cmd)
	return cmd
}

func (a *app) recordsUpdateCommand() *cobra.Command {
	flags := &writeFlags{}
	cmd := &cobra.Command{
		Use:     "update <collection> <id> [field=value | field:=json ...]",
		Short:   "Change fields of a record",
		Example: "  pbctl records update posts abc123 status=done\n  pbctl records update posts abc123 tags+=urgent views+:=1 'files-=old.pdf'",
		Args:    minimumArgs(2, "records update <collection> <id> [field=value ...]"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			if err := a.confirm("update "+arguments[0]+"/"+arguments[1], false); err != nil {
				return err
			}
			return a.writeRecord(cmd.Context(), http.MethodPatch, collectionPath(arguments[0], "records", arguments[1]), flags, arguments[2:])
		},
	}
	flags.register(cmd)
	return cmd
}

func (a *app) writeRecord(ctx context.Context, method, path string, flags *writeFlags, assignments []string) error {
	client, err := a.pb()
	if err != nil {
		return err
	}
	fields, err := a.objectFrom(flags.data, assignments)
	if err != nil {
		return err
	}
	if len(fields) == 0 && len(flags.uploads) == 0 {
		return usagef("nothing to send: pass field=value pairs, --data or --file")
	}
	request := pb.Request{Method: method, Path: path, Query: url.Values{}}
	setIfPresent(request.Query, "expand", flags.expand)
	setIfPresent(request.Query, "fields", flags.fields)
	if len(flags.uploads) > 0 {
		request.Body, request.ContentType, err = multipartBody(fields, flags.uploads)
	} else {
		request.Body, err = json.Marshal(fields)
		request.ContentType = "application/json"
	}
	if err != nil {
		return err
	}
	record := map[string]any{}
	if err := a.sendForObject(ctx, client, request, &record); err != nil {
		return err
	}
	return a.printWritten(record)
}

func (a *app) sendForObject(ctx context.Context, client *pb.Client, request pb.Request, target any) error {
	response, err := client.Do(ctx, request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode the response of %s %s: %w", request.Method, request.Path, err)
	}
	return nil
}

func (a *app) printWritten(record map[string]any) error {
	if id, hasID := record["id"].(string); a.quietFlag && hasID {
		a.printer.Line("%s", id)
		return nil
	}
	return a.printer.Object(record)
}

func (a *app) recordsDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <collection> <id>...",
		Short: "Delete records (needs --yes)",
		Args:  minimumArgs(2, "records delete <collection> <id>..."),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			collection, ids := arguments[0], arguments[1:]
			if err := a.confirm(fmt.Sprintf("delete %d record(s) from %s", len(ids), collection), true); err != nil {
				return err
			}
			client, err := a.pb()
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := client.JSON(cmd.Context(), http.MethodDelete, collectionPath(collection, "records", id), nil, nil, nil); err != nil {
					return err
				}
				a.printer.Line("deleted %s/%s", collection, id)
			}
			return nil
		},
	}
}

func (a *app) recordsImportCommand() *cobra.Command {
	var upsert bool
	cmd := &cobra.Command{
		Use:   "import <collection> <file.jsonl | file.json | ->",
		Short: "Create records from JSON lines or a JSON array, in transactional batches",
		Long:  "Each batch of 50 is one transaction: a failing record rolls its batch back.\nWith --upsert a record whose id already exists is replaced.\nThe server must allow batch requests (settings batch.enabled).",
		Args:  exactArgs(2, "records import <collection> <file>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			source := arguments[1]
			if source != "-" {
				source = "@" + source
			}
			raw, err := a.readSource(source)
			if err != nil {
				return err
			}
			records, err := parseRecords(raw)
			if err != nil {
				return err
			}
			if err := a.confirm(fmt.Sprintf("import %d record(s) into %s", len(records), arguments[0]), upsert); err != nil {
				return err
			}
			return a.importRecords(cmd.Context(), arguments[0], records, upsert)
		},
	}
	cmd.Flags().BoolVar(&upsert, "upsert", false, "replace records whose id already exists (needs --yes)")
	return cmd
}

func parseRecords(raw []byte) ([]map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	records := []map[string]any{}
	if bytes.HasPrefix(trimmed, []byte("[")) {
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.UseNumber()
		if err := decoder.Decode(&records); err != nil {
			return nil, usagef("the file is not a JSON array of objects: %v", err)
		}
		return records, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	for decoder.More() {
		record := map[string]any{}
		if err := decoder.Decode(&record); err != nil {
			return nil, usagef("record %d is not a JSON object: %v", len(records)+1, err)
		}
		records = append(records, record)
	}
	return records, nil
}

func (a *app) importRecords(ctx context.Context, collection string, records []map[string]any, upsert bool) error {
	client, err := a.pb()
	if err != nil {
		return err
	}
	method := http.MethodPost
	if upsert {
		method = http.MethodPut
	}
	for start := 0; start < len(records); start += importBatchSize {
		end := min(start+importBatchSize, len(records))
		requests := make([]map[string]any, 0, end-start)
		for _, record := range records[start:end] {
			delete(record, "collectionId")
			delete(record, "collectionName")
			delete(record, "expand")
			requests = append(requests, map[string]any{"method": method, "url": collectionPath(collection, "records"), "body": record})
		}
		if err := client.JSON(ctx, http.MethodPost, "/api/batch", nil, map[string]any{"requests": requests}, nil); err != nil {
			return fmt.Errorf("records %d-%d were rolled back (%d imported before them): %w", start+1, end, start, err)
		}
	}
	a.printer.Line("imported %d record(s) into %s", len(records), collection)
	return nil
}

func (a *app) recordsBatchCommand() *cobra.Command {
	var data string
	cmd := &cobra.Command{
		Use:     "batch --data <json | @file | ->",
		Short:   "Run several create/update/upsert/delete requests in one transaction",
		Example: "  pbctl records batch -d '[{\"method\":\"POST\",\"url\":\"/api/collections/posts/records\",\"body\":{\"title\":\"a\"}},\n                           {\"method\":\"DELETE\",\"url\":\"/api/collections/posts/records/abc123\"}]' --yes",
		Args:    exactArgs(0, "records batch --data <json>"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var requests []map[string]any
			if err := a.decodeSource(data, &requests); err != nil {
				return err
			}
			if err := a.confirm(fmt.Sprintf("run a batch of %d request(s)", len(requests)), true); err != nil {
				return err
			}
			client, err := a.pb()
			if err != nil {
				return err
			}
			var results any
			if err := client.JSON(cmd.Context(), http.MethodPost, "/api/batch", nil, map[string]any{"requests": requests}, &results); err != nil {
				return err
			}
			return a.printer.Value(results)
		},
	}
	cmd.Flags().StringVarP(&data, "data", "d", "", "JSON array of {method, url, body}")
	cmd.MarkFlagRequired("data")
	return cmd
}

func (a *app) recordsWatchCommand() *cobra.Command {
	var filter string
	var window time.Duration
	var maxEvents int
	cmd := &cobra.Command{
		Use:   "watch <collection>[/<id>]",
		Short: "Print realtime create/update/delete events as JSON lines",
		Long:  "Stops after --duration or --max events. Without a terminal the default window is one minute, so an unattended run cannot hang.",
		Args:  exactArgs(1, "records watch <collection>[/<id>]"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			if window == 0 && !a.printer.Interactive {
				window = unattendedWatchWindow
			}
			ctx := cmd.Context()
			if window > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, window)
				defer cancel()
			}
			err := a.watch(ctx, realtimeTopic(arguments[0], filter), maxEvents)
			if ctx.Err() != nil {
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVarP(&filter, "filter", "f", "", "only events for records matching this filter")
	cmd.Flags().DurationVar(&window, "duration", 0, "stop after this long (default: 1m without a terminal, unlimited with one)")
	cmd.Flags().IntVar(&maxEvents, "max", 0, "stop after this many events")
	return cmd
}

func realtimeTopic(target, filter string) string {
	topic := target
	if !strings.Contains(target, "/") {
		topic += "/*"
	}
	if filter == "" {
		return topic
	}
	options, _ := json.Marshal(map[string]any{"query": map[string]string{"filter": filter}})
	return topic + "?options=" + url.QueryEscape(string(options))
}

func (a *app) watch(ctx context.Context, topic string, maxEvents int) error {
	client, err := a.pb()
	if err != nil {
		return err
	}
	stream, err := client.Do(ctx, pb.Request{Method: http.MethodGet, Path: "/api/realtime", Streaming: true})
	if err != nil {
		return err
	}
	defer stream.Body.Close()
	lines := bufio.NewScanner(stream.Body)
	lines.Buffer(make([]byte, 64<<10), largestRealtimeEvent)
	eventName, seen := "", 0
	for lines.Scan() {
		line := lines.Text()
		if name, isEvent := strings.CutPrefix(line, "event:"); isEvent {
			eventName = strings.TrimSpace(name)
			continue
		}
		payload, isData := strings.CutPrefix(line, "data:")
		if !isData {
			continue
		}
		if eventName == "PB_CONNECT" {
			if err := subscribe(ctx, client, payload, topic); err != nil {
				return err
			}
			a.printer.Note("watching %s", topic)
			continue
		}
		a.printer.Line("%s", strings.TrimSpace(payload))
		if seen++; maxEvents > 0 && seen >= maxEvents {
			return nil
		}
	}
	return lines.Err()
}

func subscribe(ctx context.Context, client *pb.Client, connectPayload, topic string) error {
	var connection struct {
		ClientID string `json:"clientId"`
	}
	if err := json.Unmarshal([]byte(connectPayload), &connection); err != nil {
		return fmt.Errorf("unexpected realtime handshake: %w", err)
	}
	subscription := map[string]any{"clientId": connection.ClientID, "subscriptions": []string{topic}}
	return client.JSON(ctx, http.MethodPost, "/api/realtime", nil, subscription, nil)
}

func requestedColumns(fields string) []string {
	if fields == "" || strings.ContainsAny(fields, "*:") {
		return nil
	}
	columns := []string{}
	for _, field := range strings.Split(fields, ",") {
		columns = append(columns, strings.TrimSpace(field))
	}
	return columns
}
