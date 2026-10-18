package cmd

import (
	"errors"
	"net/http"
	"testing"

	"github.com/sriharip316/weavectl/internal/client"
)

// friendlyMessage renders errors kubectl-style; error strings themselves
// stay lowercase and undecorated.
func TestFriendlyMessage(t *testing.T) {
	notFound := &client.APIError{StatusCode: http.StatusNotFound}
	forbidden := &client.APIError{StatusCode: http.StatusForbidden, Op: "GET /v1/schema"}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{
			notFoundWrap(notFound, "collection", "Article"),
			`Error from server (NotFound): collection "Article" not found`,
		},
		{
			notFoundWrap(errors.New("other failure"), "collection", "Article"),
			"Error: other failure",
		},
		{
			newServerError("AlreadyExists", "%s %q already exists", "collection", "Article"),
			`Error from server (AlreadyExists): collection "Article" already exists`,
		},
		{
			forbidden,
			"Error from server (Forbidden): the server responded with status 403 (Forbidden)",
		},
		{
			errors.Join(errors.New("wrapper"), forbidden),
			"Error from server (Forbidden): wrapper\nthe server responded with status 403 (Forbidden)",
		},
		{
			errors.New("boom"),
			"Error: boom",
		},
	} {
		if got := friendlyMessage(tc.err); got != tc.want {
			t.Errorf("friendlyMessage(%v)\n got  %s\n want %s", tc.err, got, tc.want)
		}
	}
}

func TestResolveType(t *testing.T) {
	for input, want := range map[string]string{
		"collection":  "collections",
		"COLLECTIONS": "collections",
		"role":        "roles",
		"roles":       "roles",
		"user":        "users",
		"users":       "users",
		"alias":       "aliases",
		"aliases":     "aliases",
		"tenant":      "tenants",
		"tenants":     "tenants",
		"shard":       "shards",
		"shards":      "shards",
		"node":        "nodes",
		"nodes":       "nodes",
	} {
		got, err := resolveType(input)
		if err != nil || got != want {
			t.Errorf("resolveType(%q) = %q, %v; want %q", input, got, err, want)
		}
	}

	if _, err := resolveType("unknown"); err == nil {
		t.Error("expected error for unknown resource type")
	}
}
