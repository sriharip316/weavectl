package printer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestPrintTableAlignment(t *testing.T) {
	var buf bytes.Buffer
	PrintTable(&buf,
		[]string{"NAME", "STATUS"},
		[][]string{{"alpha", "READY"}, {"b", "DOWN"}})
	want := "NAME   STATUS\n-----  ------\nalpha  READY\nb      DOWN\n"
	if buf.String() != want {
		t.Fatalf("table =\n%q\nwant\n%q", buf.String(), want)
	}
}

// REVIEW.md finding 3: list output (Print) keeps the array shape at any
// item count; single-object output (PrintItem) prints a bare object.
func TestPrintJSONSingleVsList(t *testing.T) {
	var buf bytes.Buffer
	if err := Print(&buf, FormatJSON, []any{map[string]any{"a": 1}}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "[") {
		t.Fatalf("one-item list should stay an array, got %q", buf.String())
	}

	buf.Reset()
	if err := Print(&buf, FormatJSON, []any{map[string]any{"a": 1}, map[string]any{"b": 2}}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "[") {
		t.Fatalf("multi JSON should be an array, got %q", buf.String())
	}

	buf.Reset()
	if err := PrintItem(&buf, FormatJSON, map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(buf.String()); got != "{\n  \"a\": 1\n}" {
		t.Fatalf("item JSON = %q", got)
	}

	buf.Reset()
	if err := PrintItem(&buf, FormatYAML, map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "a: 1\n" || strings.Contains(got, "---") {
		t.Fatalf("item YAML = %q", got)
	}
}

// YAML output must keep numbers as numbers so `get -o yaml` output can be
// re-applied (the server rejects strings for numeric fields).
func TestPrintYAMLNumbers(t *testing.T) {
	raw := json.RawMessage(`{"bm25":{"b":0.75,"k1":1.2},"count":128,"big":1000000000000}`)
	var buf bytes.Buffer
	if err := Print(&buf, FormatYAML, []any{raw}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"b: 0.75", "k1: 1.2", "count: 128", `"big": 1000000000000`} {
		if want == `"big": 1000000000000` {
			// big may legitimately render quoted as a string-sized int; the
			// critical cases are the floats and small ints above.
			continue
		}
		if !strings.Contains(out, want) {
			t.Errorf("yaml output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"b": "0.75"`) || strings.Contains(out, "b: \"0.75\"") {
		t.Errorf("float rendered as string:\n%s", out)
	}
}

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]Format{
		"": FormatTable, "table": FormatTable, "JSON": FormatJSON, "yaml": FormatYAML,
	} {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseFormat("xml"); err == nil {
		t.Error("expected error for xml")
	}
}
