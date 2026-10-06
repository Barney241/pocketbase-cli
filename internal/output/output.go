package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"unicode/utf8"
)

type Format string

const (
	Table Format = "table"
	JSON  Format = "json"
	JSONL Format = "jsonl"
)

const DefaultMaxCell = 120

type Printer struct {
	Out         io.Writer
	Err         io.Writer
	Format      Format
	MaxCell     int
	Interactive bool
	Quiet       bool
}

func ParseFormat(value string) (Format, error) {
	switch Format(strings.ToLower(value)) {
	case Table, "":
		return Table, nil
	case JSON:
		return JSON, nil
	case JSONL:
		return JSONL, nil
	}
	return "", fmt.Errorf("unknown output format %q (use table, json or jsonl)", value)
}

func (p *Printer) Note(format string, arguments ...any) {
	if p.Quiet {
		return
	}
	fmt.Fprintf(p.Err, format+"\n", arguments...)
}

func (p *Printer) Line(format string, arguments ...any) {
	fmt.Fprintf(p.Out, format+"\n", arguments...)
}

func (p *Printer) Value(value any) error {
	encoder := json.NewEncoder(p.Out)
	encoder.SetEscapeHTML(false)
	if p.Interactive {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(value)
}

func (p *Printer) List(envelope any, columns []string, rows []map[string]any) error {
	switch p.Format {
	case JSON:
		return p.Value(envelope)
	case JSONL:
		if raw, isRawList := envelope.([]map[string]any); isRawList {
			return p.lines(raw)
		}
		return p.lines(rows)
	}
	return p.table(columns, rows)
}

func (p *Printer) Object(object map[string]any) error {
	if p.Format != Table {
		return p.Value(object)
	}
	for _, key := range OrderedKeys([]map[string]any{object}) {
		fmt.Fprintf(p.Out, "%s: %s\n", key, Cell(object[key], 0))
	}
	return nil
}

func (p *Printer) Any(value any) error {
	if object, isObject := value.(map[string]any); isObject {
		return p.Object(object)
	}
	return p.Value(value)
}

func (p *Printer) lines(rows []map[string]any) error {
	encoder := json.NewEncoder(p.Out)
	encoder.SetEscapeHTML(false)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

func (p *Printer) table(columns []string, rows []map[string]any) error {
	if len(columns) == 0 {
		columns = OrderedKeys(rows)
	}
	if len(rows) == 0 {
		return nil
	}
	truncated := false
	writeRow, flush := p.rowWriter()
	writeRow(columns)
	for _, row := range rows {
		cells := make([]string, len(columns))
		for index, column := range columns {
			full := Cell(Lookup(row, column), 0)
			cells[index] = Truncate(full, p.MaxCell)
			truncated = truncated || cells[index] != full
		}
		writeRow(cells)
	}
	if err := flush(); err != nil {
		return err
	}
	if truncated {
		p.Note("long values were cut at %d characters; add --full or -o json to see them whole", p.MaxCell)
	}
	return nil
}

func (p *Printer) rowWriter() (func([]string), func() error) {
	if !p.Interactive {
		return func(cells []string) { fmt.Fprintln(p.Out, strings.Join(cells, "\t")) }, func() error { return nil }
	}
	aligned := tabwriter.NewWriter(p.Out, 0, 4, 2, ' ', 0)
	return func(cells []string) { fmt.Fprintln(aligned, strings.Join(cells, "\t")) }, aligned.Flush
}

func Lookup(row map[string]any, column string) any {
	if value, direct := row[column]; direct {
		return value
	}
	var current any = row
	for part := range strings.SplitSeq(column, ".") {
		object, isObject := current.(map[string]any)
		if !isObject {
			return nil
		}
		current = object[part]
	}
	return current
}

var trailingColumns = map[string]int{"created": 1, "updated": 2, "expand": 3}

var hiddenColumns = map[string]bool{"collectionId": true, "collectionName": true}

func OrderedKeys(rows []map[string]any) []string {
	seen := map[string]bool{}
	keys := []string{}
	for _, row := range rows {
		for key := range row {
			if !seen[key] && !hiddenColumns[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	sort.Slice(keys, func(left, right int) bool {
		return columnRank(keys[left]) < columnRank(keys[right])
	})
	return keys
}

func columnRank(key string) string {
	if key == "id" {
		return "0"
	}
	if position, trailing := trailingColumns[key]; trailing {
		return fmt.Sprintf("2%d", position)
	}
	return "1" + key
}

var cellEscaper = strings.NewReplacer("\r\n", `\n`, "\n", `\n`, "\r", `\n`, "\t", `\t`)

func Cell(value any, limit int) string {
	var text string
	switch typed := value.(type) {
	case nil:
		text = ""
	case string:
		text = cellEscaper.Replace(typed)
	case json.Number:
		text = typed.String()
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			text = fmt.Sprint(typed)
		} else {
			text = string(encoded)
		}
	}
	return Truncate(text, limit)
}

func Truncate(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return fmt.Sprintf("%s…(+%d)", string(runes[:limit]), len(runes)-limit)
}
