package manifest

import (
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
