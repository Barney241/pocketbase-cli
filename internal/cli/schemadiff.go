package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barney241/pocketbase-cli/internal/output"
)

var collectionKeysIgnoredInDiff = map[string]bool{
	"id": true, "created": true, "updated": true, "fields": true, "indexes": true, "oauth2": true,
	"authToken": true, "passwordResetToken": true, "emailChangeToken": true, "verificationToken": true, "fileToken": true,
}

func (a *app) collectionsDiffCommand() *cobra.Command {
	var includeSystem bool
	cmd := &cobra.Command{
		Use:     "diff <other-profile> [collection...]",
		Short:   "Compare the schema of this profile with another one",
		Long:    "Lines starting with + exist only in the selected profile, - only in the other one, ~ differ.\nCollections and fields are matched by name, so ids may differ between instances.",
		Example: "  pbctl -p local collections diff prod\n  pbctl -p local collections diff prod posts users",
		Args:    minimumArgs(1, "collections diff <other-profile> [collection...]"),
		RunE: func(cmd *cobra.Command, arguments []string) error {
			client, err := a.pb()
			if err != nil {
				return err
			}
			other, err := a.clientForProfile(arguments[0])
			if err != nil {
				return err
			}
			mine, err := a.fetchCollections(cmd.Context(), client)
			if err != nil {
				return err
			}
			theirs, err := a.fetchCollections(cmd.Context(), other)
			if err != nil {
				return fmt.Errorf("profile %s: %w", arguments[0], err)
			}
			differences := diffSchemas(indexCollections(mine, includeSystem, arguments[1:]), indexCollections(theirs, includeSystem, arguments[1:]))
			if a.printer.Format != output.Table {
				return a.printer.Value(differences)
			}
			if len(differences) == 0 {
				a.printer.Note("schemas match")
				return nil
			}
			a.printer.Line("%s", strings.Join(differences, "\n"))
			return nil
		},
	}
	cmd.Flags().BoolVar(&includeSystem, "system", false, "include system collections")
	return cmd
}

func indexCollections(collections []map[string]any, includeSystem bool, only []string) map[string]map[string]any {
	wanted := map[string]bool{}
	for _, name := range only {
		wanted[name] = true
	}
	if !includeSystem && len(only) == 0 {
		collections = withoutSystemCollections(collections)
	}
	indexed := map[string]map[string]any{}
	for _, collection := range collections {
		name := fmt.Sprint(collection["name"])
		if len(wanted) == 0 || wanted[name] {
			indexed[name] = collection
		}
	}
	return indexed
}

func diffSchemas(mine, theirs map[string]map[string]any) []string {
	differences := []string{}
	for _, name := range unionOfKeys(mine, theirs) {
		left, inMine := mine[name]
		right, inTheirs := theirs[name]
		switch {
		case !inTheirs:
			differences = append(differences, fmt.Sprintf("+ %s (%v)", name, left["type"]))
		case !inMine:
			differences = append(differences, fmt.Sprintf("- %s (%v)", name, right["type"]))
		default:
			if changes := diffCollection(left, right); len(changes) > 0 {
				differences = append(differences, "~ "+name)
				differences = append(differences, changes...)
			}
		}
	}
	return differences
}

func diffCollection(mine, theirs map[string]any) []string {
	changes := []string{}
	myFields, theirFields := fieldsByName(mine), fieldsByName(theirs)
	for _, name := range unionOfKeys(myFields, theirFields) {
		left, inMine := myFields[name]
		right, inTheirs := theirFields[name]
		switch {
		case !inTheirs:
			changes = append(changes, fmt.Sprintf("    + field %s (%v)", name, left["type"]))
		case !inMine:
			changes = append(changes, fmt.Sprintf("    - field %s (%v)", name, right["type"]))
		default:
			changes = append(changes, diffValues("    ~ field "+name+" ", left, right, fieldCoreKeysExceptType)...)
		}
	}
	changes = append(changes, diffValues("    ~ ", mine, theirs, collectionKeysIgnoredInDiff)...)
	if !reflect.DeepEqual(normalizedIndexes(mine), normalizedIndexes(theirs)) {
		changes = append(changes, "    ~ indexes differ")
	}
	return changes
}

var fieldCoreKeysExceptType = map[string]bool{"id": true, "name": true, "collectionId": true}

func diffValues(prefix string, mine, theirs map[string]any, ignored map[string]bool) []string {
	changes := []string{}
	for _, key := range unionOfKeys(mine, theirs) {
		if ignored[key] {
			continue
		}
		left, right := canonical(mine[key]), canonical(theirs[key])
		if left != right {
			changes = append(changes, fmt.Sprintf("%s%s: %s -> other: %s", prefix, key, left, right))
		}
	}
	return changes
}

func canonical(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func fieldsByName(collection map[string]any) map[string]map[string]any {
	indexed := map[string]map[string]any{}
	for _, field := range fieldsOf(collection) {
		indexed[fmt.Sprint(field["name"])] = field
	}
	return indexed
}

func normalizedIndexes(collection map[string]any) []string {
	raw, _ := collection["indexes"].([]any)
	indexes := make([]string, 0, len(raw))
	for _, index := range raw {
		indexes = append(indexes, strings.Join(strings.Fields(fmt.Sprint(index)), " "))
	}
	sort.Strings(indexes)
	return indexes
}

func unionOfKeys[V any](left, right map[string]V) []string {
	seen := map[string]bool{}
	for key := range left {
		seen[key] = true
	}
	for key := range right {
		seen[key] = true
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
