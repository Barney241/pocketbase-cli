package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Barney241/pocketbase-cli/internal/output"
)

func TestFilterPlaceholdersAreQuotedSoValuesCannotBreakOut(t *testing.T) {
	cases := []struct {
		filter     string
		parameters []string
		want       string
	}{
		{"title = {:t}", []string{"t=O'Brien"}, `title = 'O\'Brien'`},
		{"title = {:t}", []string{`t=a\' || id != '`}, `title = 'a\\\' || id != \''`},
		{"views > {:n} && done = {:d} && owner = {:o}", []string{"n:=5", "d:=true", "o:=null"}, "views > 5 && done = true && owner = null"},
		{"a = {:x} || b = {:x}", []string{"x=1"}, "a = '1' || b = '1'"},
		{"status = 'open'", nil, "status = 'open'"},
	}
	for _, testCase := range cases {
		got, err := bindFilter(testCase.filter, testCase.parameters)
		if err != nil || got != testCase.want {
			t.Errorf("bindFilter(%q, %v) = %q, %v; want %q", testCase.filter, testCase.parameters, got, err, testCase.want)
		}
	}
	if _, err := bindFilter("title = {:missing}", nil); err == nil {
		t.Error("a placeholder without a value was accepted")
	}
}

func TestAssignmentsBuildNestedObjectsOnTopOfData(t *testing.T) {
	a := &app{stdin: strings.NewReader(`{"title":"from stdin","meta":{"keep":1}}`)}
	object, err := a.objectFrom("-", []string{"title=override", "views:=3", "tags:=[\"a\"]", "meta.appName=Demo", "empty=", "tags+=b"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(object)
	want := `{"empty":"","meta":{"appName":"Demo","keep":1},"tags":["a"],"tags+":"b","title":"override","views":3}`
	if string(encoded) != want {
		t.Fatalf("object = %s\nwant     %s", encoded, want)
	}
	for _, invalid := range []string{"novalue", "=x", "n:=notjson"} {
		if _, err := a.objectFrom("", []string{invalid}); err == nil {
			t.Errorf("assignment %q was accepted", invalid)
		}
	}
}

func TestRecordsParseFromJSONLinesAndFromAnArray(t *testing.T) {
	fromLines, err := parseRecords([]byte("{\"a\":1}\n\n{\"a\":2}\n"))
	if err != nil || len(fromLines) != 2 {
		t.Fatalf("lines: %v %v", fromLines, err)
	}
	fromArray, err := parseRecords([]byte(` [{"a":1},{"a":2},{"a":3}] `))
	if err != nil || len(fromArray) != 3 {
		t.Fatalf("array: %v %v", fromArray, err)
	}
	if _, err := parseRecords([]byte(`{"a":1} 5`)); err == nil {
		t.Fatal("a non-object line was accepted")
	}
}

func TestLogShortcutsBecomeAFilter(t *testing.T) {
	conditions, err := (&logFlags{level: "warn", status: "5xx"}).conditions()
	if err != nil {
		t.Fatal(err)
	}
	if got := joinConditions(conditions, "data.url ~ '/api/chat'"); got != "level >= 4 && (data.status >= 500 && data.status < 600) && (data.url ~ '/api/chat')" {
		t.Fatalf("filter = %s", got)
	}
	for _, invalid := range []*logFlags{{level: "loud"}, {status: "9xx"}, {status: "abc"}, {since: "soon"}} {
		if _, err := invalid.conditions(); err == nil {
			t.Errorf("%+v was accepted", invalid)
		}
	}
	if age, err := parseAge("2d"); err != nil || age.Hours() != 48 {
		t.Fatalf("2d = %v %v", age, err)
	}
}

func TestSchemaDiffMatchesByNameAndIgnoresIds(t *testing.T) {
	mine := map[string]map[string]any{
		"posts": {"id": "a1", "name": "posts", "type": "base", "listRule": "", "fields": []any{
			map[string]any{"id": "f1", "name": "title", "type": "text", "max": json.Number("200")},
			map[string]any{"id": "f2", "name": "draft", "type": "bool"},
		}},
		"only_mine": {"name": "only_mine", "type": "base"},
	}
	theirs := map[string]map[string]any{
		"posts": {"id": "b9", "name": "posts", "type": "base", "listRule": nil, "fields": []any{
			map[string]any{"id": "z7", "name": "title", "type": "text", "max": json.Number("100")},
			map[string]any{"id": "z8", "name": "legacy", "type": "number"},
		}},
		"only_theirs": {"name": "only_theirs", "type": "view"},
	}
	want := []string{
		"+ only_mine (base)",
		"- only_theirs (view)",
		"~ posts",
		"    + field draft (bool)",
		"    - field legacy (number)",
		"    ~ field title max: 200 -> other: 100",
		`    ~ listRule: "" -> other: null`,
	}
	if got := diffSchemas(mine, theirs); !reflect.DeepEqual(got, want) {
		t.Fatalf("diff =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if same := diffSchemas(mine, mine); len(same) != 0 {
		t.Fatalf("a schema differs from itself: %v", same)
	}
}

func TestTablesAreTabSeparatedAndOnlyTablesAreCut(t *testing.T) {
	rows := []map[string]any{{"id": "a1", "title": "line one\nline two\tend", "body": strings.Repeat("x", 30), "collectionName": "posts"}}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	printer := &output.Printer{Out: stdout, Err: stderr, Format: output.Table, MaxCell: 10}
	if err := printer.List(rows, nil, rows); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "id\tbody\ttitle\na1\txxxxxxxxxx…(+20)\tline one\\n…(+13)\n" {
		t.Fatalf("table = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--full") {
		t.Fatalf("no truncation note: %q", stderr.String())
	}
	stdout.Reset()
	printer.Format = output.JSONL
	printer.List(rows, nil, rows)
	if !strings.Contains(stdout.String(), strings.Repeat("x", 30)) || !strings.Contains(stdout.String(), "collectionName") {
		t.Fatalf("jsonl lost data: %q", stdout.String())
	}
}
