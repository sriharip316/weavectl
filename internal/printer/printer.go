// Package printer renders objects as a plain table (default), JSON, or YAML.
package printer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
)

func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(s) {
	case "", "table", "wide":
		return FormatTable, nil
	case "json":
		return FormatJSON, nil
	case "yaml", "yml":
		return FormatYAML, nil
	}
	return "", fmt.Errorf("unknown output format %q (want table, json or yaml)", s)
}

// Print writes one or many objects in the requested format. For JSON/YAML,
// items must be JSON-marshalable (map[string]any, typed structs, or
// json.RawMessage); the output is API-native so it round-trips with apply.
func Print(out io.Writer, format Format, items []any) error {
	switch format {
	case FormatJSON:
		return printJSON(out, items)
	case FormatYAML:
		return printYAML(out, items)
	default:
		return fmt.Errorf("table output requires a Table, use PrintTable")
	}
}

// PrintTable writes rows under headers using column alignment. Rows cells
// are strings or fmt.Stringer-compatible values.
func PrintTable(out io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	writeRow := func(cells []string) {
		parts := make([]string, len(cells))
		for i, cell := range cells {
			if i == len(cells)-1 {
				parts[i] = cell // no trailing padding on the last column
			} else {
				parts[i] = cell + strings.Repeat(" ", widths[i]-len(cell))
			}
		}
		_, _ = fmt.Fprintln(out, strings.TrimRight(strings.Join(parts, "  "), " "))
	}

	writeRow(headers)
	sep := make([]string, len(headers))
	for i := range sep {
		sep[i] = strings.Repeat("-", widths[i])
	}
	writeRow(sep)
	for _, row := range rows {
		writeRow(row)
	}
}

func printJSON(out io.Writer, items []any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	var v any = items
	if len(items) == 1 {
		v = items[0]
	}
	return enc.Encode(v)
}

func printYAML(out io.Writer, items []any) error {
	for _, item := range items {
		v, err := decodeForYAML(item)
		if err != nil {
			return err
		}
		buf, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		_, _ = out.Write(buf)
		if len(items) > 1 {
			_, _ = fmt.Fprintln(out, "---")
		}
	}
	return nil
}

// decodeForYAML round-trips the item through JSON so json.RawMessage and
// typed structs all become plain maps, with json.Decoder.UseNumber keeping
// integers readable (no 1.40e+02 artifacts).
func decodeForYAML(item any) (any, error) {
	if s, ok := item.(string); ok {
		return s, nil
	}
	buf, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return normalizeForYAML(v), nil
}

func normalizeForYAML(v any) any {
	switch t := v.(type) {
	case map[string]any:
		// yaml.v3 sorts map keys itself; keep values normalized recursively.
		for k, val := range t {
			t[k] = normalizeForYAML(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = normalizeForYAML(val)
		}
		return t
	case json.Number:
		// yaml.v3 would marshal json.Number (a string type) as a quoted
		// string; convert back to a real number so `get -o yaml` output
		// re-applies cleanly.
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	}
	return v
}

// SortedKeys returns map keys in order, handy for deterministic tables.
func SortedKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
