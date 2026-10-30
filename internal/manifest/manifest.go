// Package manifest loads object definitions from files (or stdin) and
// identifies what kind of Weaviate object each document describes.
//
// Manifests are raw Weaviate definitions — the same JSON the API accepts
// and returns — so `weavectl get collection X -o yaml` output can be
// edited and fed straight back to `weavectl apply -f`.
package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind identifies the type of Weaviate object a manifest describes.
type Kind string

const (
	KindCollection Kind = "collection"
	KindRole       Kind = "role"
	KindUser       Kind = "user"
	KindAlias      Kind = "alias"
	KindTenants    Kind = "tenants"
	KindBackup     Kind = "backup"
	KindRestore    Kind = "restore"
	KindUnknown    Kind = ""
)

// Document is one object definition plus its detected kind.
type Document struct {
	Kind   Kind
	Name   string // primary object name, used for delete and logging
	Object map[string]any
}

// Manifest is the tenants composite: a collection plus tenant list.
type TenantsDoc struct {
	Collection string
	Tenants    []map[string]any
}

// MaxManifestBytes caps how much of a manifest the loader will buffer,
// so piping an endless (or accidentally huge) stream into stdin cannot
// exhaust process memory.
var MaxManifestBytes int64 = 32 << 20 // 32 MiB

// readAllLimited buffers r up to MaxManifestBytes and errors beyond it.
func readAllLimited(r io.Reader, what string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxManifestBytes {
		return nil, fmt.Errorf("%s exceeds the %d byte limit", what, MaxManifestBytes)
	}
	return data, nil
}

// Load reads a manifest file ("-" for stdin) and returns its documents.
// JSON files hold a single object; YAML files may hold multiple documents
// separated by ---.
func Load(path string) ([]Document, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = readAllLimited(os.Stdin, "manifest from stdin")
	} else {
		var f *os.File
		f, err = os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		data, err = readAllLimited(f, fmt.Sprintf("manifest %s", path))
	}
	if err != nil {
		return nil, err
	}
	return Parse(data, strings.HasSuffix(strings.ToLower(path), ".json"))
}

// Parse turns manifest bytes into documents. jsonFormat forces single-doc
// JSON parsing; otherwise YAML is assumed (JSON is valid YAML, but using
// the JSON path preserves exact field data for the common case).
func Parse(data []byte, jsonFormat bool) ([]Document, error) {
	raws, err := splitDocuments(data, jsonFormat)
	if err != nil {
		return nil, err
	}

	var docs []Document
	for i, raw := range raws {
		if strings.TrimSpace(string(raw)) == "" {
			continue
		}
		var obj map[string]any
		if err := yaml.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		if obj == nil {
			continue
		}
		doc, err := classify(obj)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		docs = append(docs, doc)
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("no documents found in manifest")
	}
	return docs, nil
}

func splitDocuments(data []byte, jsonFormat bool) ([][]byte, error) {
	if jsonFormat {
		if !json.Valid(data) {
			return nil, fmt.Errorf("invalid JSON")
		}
		return [][]byte{data}, nil
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	var docs [][]byte
	for {
		var node any
		err := dec.Decode(&node)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if node == nil {
			continue
		}
		// Re-marshal each YAML document so callers get normalized bytes.
		out, err := yaml.Marshal(node)
		if err != nil {
			return nil, err
		}
		docs = append(docs, out)
	}
	return docs, nil
}

// classify detects the kind from the raw fields and extracts the name.
func classify(obj map[string]any) (Document, error) {
	// An explicit kind wins; it is not forwarded to the API.
	if k, ok := obj["kind"].(string); ok && k != "" {
		doc := Document{Kind: Kind(strings.ToLower(k)), Object: withoutKind(obj)}
		if err := doc.fillName(); err != nil {
			return doc, err
		}
		return doc, nil
	}

	switch {
	// Aliases carry both "alias" and "class"; they must be checked before
	// plain collections, which are recognized by "class" alone.
	case str(obj["alias"]) != "" && str(obj["class"]) != "":
		return Document{Kind: KindAlias, Name: str(obj["alias"]), Object: obj}, nil
	case str(obj["class"]) != "":
		return Document{Kind: KindCollection, Name: str(obj["class"]), Object: obj}, nil
	case str(obj["name"]) != "" && has(obj, "permissions"):
		return Document{Kind: KindRole, Name: str(obj["name"]), Object: obj}, nil
	case str(obj["userId"]) != "":
		return Document{Kind: KindUser, Name: str(obj["userId"]), Object: obj}, nil
	case str(obj["collection"]) != "" && has(obj, "tenants"):
		return Document{Kind: KindTenants, Name: str(obj["collection"]), Object: obj}, nil
	case str(obj["backend"]) != "" && str(obj["id"]) != "":
		// A backup definition: backend + id select the backup; restore
		// definitions look the same, so they must say "kind: Restore".
		return Document{Kind: KindBackup, Name: backupDocName(obj), Object: obj}, nil
	}
	return Document{Object: obj}, fmt.Errorf("cannot determine object kind: expected a collection (class), role (name+permissions), user (userId), alias (alias+class), tenants (collection+tenants), or backup (backend+id) definition; add a \"kind\" field to make it explicit (a restore requires \"kind: Restore\")")
}

func (d *Document) fillName() error {
	switch d.Kind {
	case KindCollection:
		d.Name = str(d.Object["class"])
	case KindRole:
		d.Name = str(d.Object["name"])
	case KindUser:
		d.Name = str(d.Object["userId"])
	case KindAlias:
		d.Name = str(d.Object["alias"])
	case KindTenants:
		d.Name = str(d.Object["collection"])
	case KindBackup, KindRestore:
		if str(d.Object["backend"]) == "" || str(d.Object["id"]) == "" {
			return fmt.Errorf("manifest with kind %q requires \"backend\" and \"id\" fields", d.Kind)
		}
		d.Name = backupDocName(d.Object)
	}
	if d.Name == "" {
		return fmt.Errorf("manifest with kind %q is missing its name field", d.Kind)
	}
	return nil
}

// backupDocName is the display name of a backup/restore document:
// "backend/id". Backups and restores are scoped by both.
func backupDocName(obj map[string]any) string {
	return str(obj["backend"]) + "/" + str(obj["id"])
}

// Backend returns the backup backend a backup/restore document targets.
func (d *Document) Backend() string {
	return str(d.Object["backend"])
}

// BackupID returns the backup id a backup/restore document refers to.
func (d *Document) BackupID() string {
	return str(d.Object["id"])
}

// Tenants returns the composite tenants view of the document.
func (d *Document) TenantsDoc() (*TenantsDoc, error) {
	if d.Kind != KindTenants {
		return nil, fmt.Errorf("document is not a tenants manifest")
	}
	list, ok := d.Object["tenants"].([]any)
	if !ok {
		return nil, fmt.Errorf("tenants manifest for %q: \"tenants\" must be a list", d.Name)
	}
	tenants := make([]map[string]any, 0, len(list))
	for i, t := range list {
		m, ok := t.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tenants manifest for %q: entry %d is not an object", d.Name, i+1)
		}
		if str(m["name"]) == "" {
			return nil, fmt.Errorf("tenants manifest for %q: entry %d has no name", d.Name, i+1)
		}
		tenants = append(tenants, m)
	}
	return &TenantsDoc{Collection: d.Name, Tenants: tenants}, nil
}

func withoutKind(obj map[string]any) map[string]any {
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		if k != "kind" {
			out[k] = v
		}
	}
	return out
}

func has(obj map[string]any, key string) bool {
	_, ok := obj[key]
	return ok
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
