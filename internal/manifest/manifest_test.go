package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseKindDetection(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		jsonFmt bool
		want    Kind
		wantNm  string
		wantErr bool
	}{
		{"collection yaml", "class: Foo\nvectorizer: none\n", false, KindCollection, "Foo", false},
		{"collection json", `{"class":"Foo","properties":[]}`, true, KindCollection, "Foo", false},
		{"role", "name: dev\npermissions:\n  - action: read_roles\n    roles: {role: '*'}\n", false, KindRole, "dev", false},
		{"user", "userId: dev-user\nroles: [dev]\n", false, KindUser, "dev-user", false},
		{"alias wins over class", "alias: news\nclass: Articles\n", false, KindAlias, "news", false},
		{"tenants", "collection: MT\ntenants:\n  - name: t1\n", false, KindTenants, "MT", false},
		{"backup by fields", "backend: filesystem\nid: snap-1\ninclude: [Articles]\n", false, KindBackup, "filesystem/snap-1", false},
		{"backup explicit kind", "kind: Backup\nbackend: s3\nid: snap-1\n", false, KindBackup, "s3/snap-1", false},
		{"restore explicit kind", "kind: restore\nbackend: filesystem\nid: snap-1\noverwriteAlias: true\n", false, KindRestore, "filesystem/snap-1", false},
		{"backup without id", "backend: filesystem\n", false, KindUnknown, "", true},
		{"restore kind without backend", "kind: Restore\nid: snap-1\n", false, KindUnknown, "", true},
		{"explicit kind", "kind: alias\nalias: a\nclass: C\n", false, KindAlias, "a", false},
		{"explicit kind hides from api", "kind: collection\nclass: C\n", false, KindCollection, "C", false},
		{"unknown shape", "foo: bar\n", false, KindUnknown, "", true},
		{"empty class", "class: \"\"\n", false, KindUnknown, "", true},
		{"empty userId", "userId: \"\"\n", false, KindUnknown, "", true},
		{"alias without class", "alias: myalias\n", false, KindUnknown, "", true},
		{"explicit kind missing name", "kind: collection\nfoo: bar\n", false, KindUnknown, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs, err := Parse([]byte(tt.input), tt.jsonFmt)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", docs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(docs) != 1 {
				t.Fatalf("want 1 doc, got %d", len(docs))
			}
			if docs[0].Kind != tt.want {
				t.Errorf("kind = %q, want %q", docs[0].Kind, tt.want)
			}
			if docs[0].Name != tt.wantNm {
				t.Errorf("name = %q, want %q", docs[0].Name, tt.wantNm)
			}
			if _, hasKindField := docs[0].Object["kind"]; tt.wantErr == false && hasKindField {
				t.Errorf("kind field must not be forwarded to the API")
			}
		})
	}
}

func TestParseMultiDocYAML(t *testing.T) {
	in := `class: A
---
name: r1
permissions: []
---
# a comment-only document is skipped
userId: u1
`
	docs, err := Parse([]byte(in), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("want 3 docs, got %d", len(docs))
	}
	wantKinds := []Kind{KindCollection, KindRole, KindUser}
	for i, want := range wantKinds {
		if docs[i].Kind != want {
			t.Errorf("doc %d kind = %q, want %q", i, docs[i].Kind, want)
		}
	}
}

func TestParseJSONInvalid(t *testing.T) {
	if _, err := Parse([]byte(`{"class":`), true); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseEmpty(t *testing.T) {
	if _, err := Parse([]byte(""), false); err == nil {
		t.Fatal("expected error for empty manifest")
	}
}

func TestTenantsDoc(t *testing.T) {
	docs, err := Parse([]byte("collection: MT\ntenants:\n  - name: t1\n    activityStatus: ACTIVE\n  - {}\n"), false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := docs[0].TenantsDoc(); err == nil {
		t.Fatal("expected error for tenant entry without name")
	}

	docs, err = Parse([]byte("collection: MT\ntenants:\n  - name: t1\n"), false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	td, err := docs[0].TenantsDoc()
	if err != nil {
		t.Fatalf("TenantsDoc: %v", err)
	}
	if td.Collection != "MT" || len(td.Tenants) != 1 || td.Tenants[0]["name"] != "t1" {
		t.Fatalf("bad tenants doc: %+v", td)
	}
}

func TestLoadStdinRejectsOversizedManifest(t *testing.T) {
	orig := MaxManifestBytes
	MaxManifestBytes = 1024
	defer func() { MaxManifestBytes = orig }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1025)) // one byte over the cap
		_ = w.Close()
	}()

	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	_, err = Load("-")
	if err == nil || !strings.Contains(err.Error(), "stdin") || !strings.Contains(err.Error(), "1024 byte limit") {
		t.Fatalf("want size-limit error for stdin, got %v", err)
	}
}

func TestLoadFileRejectsOversizedManifest(t *testing.T) {
	orig := MaxManifestBytes
	MaxManifestBytes = 1024
	defer func() { MaxManifestBytes = orig }()

	path := filepath.Join(t.TempDir(), "big.yaml")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1025), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "1024 byte limit") {
		t.Fatalf("want size-limit error naming the file, got %v", err)
	}
}

func TestLoadFileUnderLimitStillParses(t *testing.T) {
	orig := MaxManifestBytes
	MaxManifestBytes = 1024
	defer func() { MaxManifestBytes = orig }()

	path := filepath.Join(t.TempDir(), "ok.json")
	if err := os.WriteFile(path, []byte(`{"class":"Articles"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	docs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Kind != KindCollection {
		t.Fatalf("unexpected docs: %+v", docs)
	}
}
