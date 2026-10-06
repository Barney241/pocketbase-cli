package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barney241/pocketbase-cli/internal/output"
	"github.com/Barney241/pocketbase-cli/internal/pb"
)

var ruleNames = []string{"listRule", "viewRule", "createRule", "updateRule", "deleteRule", "authRule", "manageRule"}

var fieldFlagNames = []string{"required", "hidden", "presentable", "system", "primaryKey"}

var fieldCoreKeys = map[string]bool{"id": true, "name": true, "type": true, "collectionId": true}

func (a *app) collectionsCommand() *cobra.Command {
	collections := &cobra.Command{Use: "collections", Aliases: []string{"collection", "c"}, Short: "Inspect and change the schema"}
	collections.AddCommand(
		a.collectionsListCommand(), a.collectionsShowCommand(), a.collectionsExportCommand(), a.collectionsDiffCommand(),
		a.collectionsScaffoldsCommand(), a.collectionsDryRunViewCommand(),
		a.collectionsCreateCommand(), a.collectionsUpdateCommand(), a.collectionsDeleteCommand(),
		a.collectionsTruncateCommand(), a.collectionsImportCommand(),
	)
	return collections
}

func (a *app) fetchCollections(ctx context.Context, client *pb.Client) ([]map[string]any, error) {
	return fetchEverything(ctx, client, "/api/collections", url.Values{"sort": {"name"}})
}

func (a *app) collectionsListCommand() *cobra.Command {
	var includeSystem bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List collections with their type and field names",
		Args:    exactArgs(0, "collections list"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			collections, err := a.fetchCollections(cmd.Context(), client)
			if err != nil {
				return err
			}
			if !includeSystem {
				collections = withoutSystemCollections(collections)
			}
			rows := make([]map[string]any, 0, len(collections))
			for _, collection := range collections {
				rows = append(rows, map[string]any{
					"name":   collection["name"],
					"type":   collection["type"],
					"fields": strings.Join(fieldNames(collection), ","),
				})
			}
			return a.printer.List(collections, []string{"name", "type", "fields"}, rows)
		},
	}
	cmd.Flags().BoolVar(&includeSystem, "system", false, "include system collections such as _superusers")
	return cmd
}

func withoutSystemCollections(collections []map[string]any) []map[string]any {
	kept := collections[:0:0]
	for _, collection := range collections {
		if isSystem, _ := collection["system"].(bool); !isSystem {
			kept = append(kept, collection)
		}
	}
	return kept
}

func fieldsOf(collection map[string]any) []map[string]any {
	raw, _ := collection["fields"].([]any)
	fields := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		if field, isObject := entry.(map[string]any); isObject {
			fields = append(fields, field)
		}
	}
	return fields
}

func fieldNames(collection map[string]any) []string {
	names := []string{}
	for _, field := range fieldsOf(collection) {
		names = append(names, fmt.Sprint(field["name"]))
	}
	return names
}

func (a *app) collectionsShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "show <collection>",
		Aliases: []string{"get", "schema"},
		Short:   "Show fields, API rules and indexes of a collection, one line each",
		Args:    exactArgs(1, "collections show <collection>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			collection := map[string]any{}
			if err := client.JSON(cmd.Context(), http.MethodGet, collectionPath(arguments[0]), nil, nil, &collection); err != nil {
				return err
			}
			if a.printer.Format != output.Table {
				return a.printer.Value(collection)
			}
			names := a.collectionNamesByID(cmd.Context(), client)
			a.printer.Line("%s", strings.Join(describeCollection(collection, names), "\n"))
			return nil
		},
	}
}

func (a *app) collectionNamesByID(ctx context.Context, client *pb.Client) map[string]string {
	names := map[string]string{}
	collections, err := fetchEverything(ctx, client, "/api/collections", url.Values{"fields": {"id,name"}})
	if err != nil {
		return names
	}
	for _, collection := range collections {
		names[fmt.Sprint(collection["id"])] = fmt.Sprint(collection["name"])
	}
	return names
}

func describeCollection(collection map[string]any, namesByID map[string]string) []string {
	lines := []string{fmt.Sprintf("%v (%v) id=%v", collection["name"], collection["type"], collection["id"])}
	lines = append(lines, "fields:")
	for _, field := range fieldsOf(collection) {
		lines = append(lines, "  "+describeField(field, namesByID))
	}
	if query, isView := collection["viewQuery"].(string); isView && query != "" {
		lines = append(lines, "viewQuery: "+query)
	}
	lines = append(lines, "rules:")
	for _, rule := range ruleNames {
		if value, present := collection[rule]; present {
			lines = append(lines, fmt.Sprintf("  %s: %s", strings.TrimSuffix(rule, "Rule"), describeRule(value)))
		}
	}
	if indexes, _ := collection["indexes"].([]any); len(indexes) > 0 {
		lines = append(lines, "indexes:")
		for _, index := range indexes {
			lines = append(lines, "  "+strings.Join(strings.Fields(fmt.Sprint(index)), " "))
		}
	}
	return lines
}

func describeRule(rule any) string {
	switch typed := rule.(type) {
	case nil:
		return "(superusers only)"
	case string:
		if typed == "" {
			return "(anyone)"
		}
		return typed
	}
	return fmt.Sprint(rule)
}

func describeField(field map[string]any, namesByID map[string]string) string {
	parts := []string{fmt.Sprint(field["name"]), fmt.Sprint(field["type"])}
	if target, isRelation := field["collectionId"].(string); isRelation && target != "" {
		parts = append(parts, "->"+firstNonEmpty(namesByID[target], target))
	}
	for _, flag := range fieldFlagNames {
		if enabled, _ := field[flag].(bool); enabled {
			parts = append(parts, flag)
		}
	}
	return strings.Join(append(parts, fieldOptions(field)...), " ")
}

func fieldOptions(field map[string]any) []string {
	options := []string{}
	for key, value := range field {
		if fieldCoreKeys[key] || isFieldFlag(key) || isZeroValue(value) {
			continue
		}
		options = append(options, key+"="+output.Cell(value, 0))
	}
	sort.Strings(options)
	return options
}

func isFieldFlag(key string) bool {
	for _, flag := range fieldFlagNames {
		if flag == key {
			return true
		}
	}
	return false
}

func isZeroValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case bool:
		return !typed
	case string:
		return typed == ""
	case json.Number:
		return typed.String() == "0"
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	}
	return false
}

func (a *app) collectionsExportCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "export [collection...]",
		Short: "Print collections as the JSON array that `collections import` accepts",
		RunE: func(cmd *cobra.Command, arguments []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			collections, err := a.fetchCollections(cmd.Context(), client)
			if err != nil {
				return err
			}
			if len(arguments) > 0 {
				collections, err = pickCollections(collections, arguments)
				if err != nil {
					return err
				}
			}
			return a.printer.Value(collections)
		},
	}
}

func pickCollections(collections []map[string]any, wanted []string) ([]map[string]any, error) {
	byName := map[string]map[string]any{}
	for _, collection := range collections {
		byName[fmt.Sprint(collection["name"])] = collection
	}
	picked := []map[string]any{}
	for _, name := range wanted {
		collection, found := byName[name]
		if !found {
			return nil, usagef("collection %q does not exist", name)
		}
		picked = append(picked, collection)
	}
	return picked, nil
}

func (a *app) collectionsScaffoldsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "scaffolds",
		Short: "Print empty base, auth and view collection templates to start a create payload from",
		Args:  exactArgs(0, "collections scaffolds"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.getAndPrint(cmd.Context(), "/api/collections/meta/scaffolds", nil)
		},
	}
}

func (a *app) getAndPrint(ctx context.Context, path string, query url.Values) error {
	client, err := a.pb()
	if err != nil {
		return err
	}
	var payload any
	if err := client.JSON(ctx, http.MethodGet, path, query, nil, &payload); err != nil {
		return err
	}
	return a.printer.Value(payload)
}

func (a *app) collectionsDryRunViewCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "dry-run-view <select query>",
		Aliases: []string{"select"},
		Short:   "Run a SELECT as a throwaway view and print its columns and up to 10 rows",
		Long:    "The server wraps the query in a SELECT, so it cannot write. It works in read-only mode.\nThe query needs an id column, e.g. SELECT id, email FROM users.",
		Args:    exactArgs(1, "collections dry-run-view \"<select query>\""),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			var result struct {
				Fields []map[string]any `json:"fields"`
				Sample []map[string]any `json:"sample"`
			}
			body := map[string]string{"query": arguments[0]}
			if err := client.JSON(cmd.Context(), http.MethodPost, "/api/collections/meta/dry-run-view", nil, body, &result); err != nil {
				return err
			}
			columns := []string{}
			for _, field := range result.Fields {
				columns = append(columns, fmt.Sprint(field["name"]))
			}
			return a.printer.List(result, columns, result.Sample)
		},
	}
}

func (a *app) collectionsCreateCommand() *cobra.Command {
	var data string
	cmd := &cobra.Command{
		Use:     "create [key=value | key:=json ...]",
		Short:   "Create a collection from a JSON definition",
		Example: "  pbctl collections create -d '{\"name\":\"notes\",\"type\":\"base\",\"fields\":[{\"name\":\"body\",\"type\":\"text\"}]}'\n  pbctl collections scaffolds   # templates to start from",
		RunE: func(cmd *cobra.Command, arguments []string) error {
			if err := a.confirm("create a collection", false); err != nil {
				return err
			}
			return a.writeCollection(cmd.Context(), http.MethodPost, "/api/collections", data, arguments)
		},
	}
	cmd.Flags().StringVarP(&data, "data", "d", "", "JSON object, @file or - for stdin")
	return cmd
}

func (a *app) collectionsUpdateCommand() *cobra.Command {
	var data string
	cmd := &cobra.Command{
		Use:     "update <collection> [key=value | key:=json ...]",
		Short:   "Change a collection: rules, fields, indexes, options",
		Long:    "Sending \"fields\" replaces the whole field list: a field left out is dropped with its data.\nStart from `pbctl collections show <name> -o json`.",
		Example: "  pbctl collections update posts listRule='@request.auth.id != \"\"' deleteRule:=null",
		Args:    minimumArgs(1, "collections update <collection> [key=value ...]"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			if err := a.confirm("change the collection "+arguments[0], true); err != nil {
				return err
			}
			return a.writeCollection(cmd.Context(), http.MethodPatch, collectionPath(arguments[0]), data, arguments[1:])
		},
	}
	cmd.Flags().StringVarP(&data, "data", "d", "", "JSON object, @file or - for stdin")
	return cmd
}

func (a *app) writeCollection(ctx context.Context, method, path, data string, assignments []string) error {
	client, err := a.pb()
	if err != nil {
		return err
	}
	definition, err := a.objectFrom(data, assignments)
	if err != nil {
		return err
	}
	if len(definition) == 0 {
		return usagef("nothing to send: pass key=value pairs or --data")
	}
	collection := map[string]any{}
	if err := client.JSON(ctx, method, path, nil, definition, &collection); err != nil {
		return err
	}
	if a.printer.Format != output.Table {
		return a.printer.Value(collection)
	}
	a.printer.Line("%s", strings.Join(describeCollection(collection, map[string]string{}), "\n"))
	return nil
}

func (a *app) collectionsDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <collection>",
		Short: "Delete a collection and all its records (needs --yes)",
		Args:  exactArgs(1, "collections delete <collection>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			return a.destroy(cmd.Context(), "delete the collection "+arguments[0]+" and all its records", http.MethodDelete, collectionPath(arguments[0]), "deleted collection "+arguments[0])
		},
	}
}

func (a *app) collectionsTruncateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "truncate <collection>",
		Short: "Delete every record of a collection, keeping the schema (needs --yes)",
		Args:  exactArgs(1, "collections truncate <collection>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			return a.destroy(cmd.Context(), "delete every record of "+arguments[0], http.MethodDelete, collectionPath(arguments[0], "truncate"), "truncated "+arguments[0])
		},
	}
}

func (a *app) destroy(ctx context.Context, action, method, path, done string) error {
	if err := a.confirm(action, true); err != nil {
		return err
	}
	client, err := a.pb()
	if err != nil {
		return err
	}
	if err := client.JSON(ctx, method, path, nil, nil, nil); err != nil {
		return err
	}
	a.printer.Line("%s", done)
	return nil
}

func (a *app) collectionsImportCommand() *cobra.Command {
	var deleteMissing bool
	cmd := &cobra.Command{
		Use:   "import <file.json | ->",
		Short: "Create or update collections from an exported JSON array (needs --yes)",
		Long:  "With --delete-missing every collection and field absent from the file is dropped with its data.",
		Args:  exactArgs(1, "collections import <file.json>"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			source := arguments[0]
			if source != "-" {
				source = "@" + source
			}
			var collections []map[string]any
			if err := a.decodeSource(source, &collections); err != nil {
				return err
			}
			action := fmt.Sprintf("import %d collection(s)", len(collections))
			if deleteMissing {
				action += " and delete every collection and field missing from the file"
			}
			if err := a.confirm(action, true); err != nil {
				return err
			}
			client, err := a.pb()
			if err != nil {
				return err
			}
			body := map[string]any{"collections": collections, "deleteMissing": deleteMissing}
			if err := client.JSON(cmd.Context(), http.MethodPut, "/api/collections/import", nil, body, nil); err != nil {
				return err
			}
			a.printer.Line("imported %d collection(s)", len(collections))
			return nil
		},
	}
	cmd.Flags().BoolVar(&deleteMissing, "delete-missing", false, "drop collections and fields that the file does not contain")
	return cmd
}
