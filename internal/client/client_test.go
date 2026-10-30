package client

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDoBearerAuthAndPath(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/", "secret") // trailing slash must be tolerated
	raw, err := c.Do(http.MethodGet, "/meta", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"ok":true}` {
		t.Fatalf("body = %s", raw)
	}
	if gotPath != "/v1/meta" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s", gotMethod)
	}
}

func TestErrorShapes(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantReason string
		wantMsg    string
	}{
		{"wrapped error array", 422, `{"error":[{"message":"boom"},{"message":"second"}]}`, "Invalid", "boom; second"},
		{"schema update shape", 422, `{"allowed":null,"error":[{"message":"immutable"}]}`, "Invalid", "immutable"},
		{"flat message", 401, `{"code":401,"message":"anonymous access not enabled"}`, "Unauthorized", "anonymous access not enabled"},
		{"empty body 404", 404, ``, "NotFound", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			c := New(srv.URL, "")
			_, err := c.Do(http.MethodGet, "/schema", nil)
			if err == nil {
				t.Fatal("expected error")
			}
			apiErr, ok := err.(*APIError)
			if !ok {
				t.Fatalf("want *APIError, got %T", err)
			}
			if apiErr.Reason() != tt.wantReason {
				t.Errorf("reason = %s, want %s", apiErr.Reason(), tt.wantReason)
			}
			if tt.wantMsg != "" && len(apiErr.Messages) == 0 {
				t.Fatalf("no messages parsed from %s", tt.body)
			}
			if tt.wantMsg != "" && strings.Join(apiErr.Messages, "; ") != tt.wantMsg {
				t.Errorf("messages = %q, want %q", strings.Join(apiErr.Messages, "; "), tt.wantMsg)
			}
			if tt.wantReason == "NotFound" != IsNotFound(err) {
				t.Errorf("IsNotFound disagrees for %d", apiErr.StatusCode)
			}
		})
	}
}

// Status checks must see through %w wrapping; a bare type assertion
// breaks as soon as a caller decorates the error.
func TestIsNotFoundAndConflictUnwrap(t *testing.T) {
	notFound := &APIError{StatusCode: http.StatusNotFound}
	conflict := &APIError{StatusCode: http.StatusConflict}
	forbidden := &APIError{StatusCode: http.StatusForbidden}
	plain := errors.New("nope")

	for _, tt := range []struct {
		name         string
		err          error
		wantNotFound bool
		wantConflict bool
	}{
		{"bare 404", notFound, true, false},
		{"bare 409", conflict, false, true},
		{"wrapped 404", fmt.Errorf("lookup failed: %w", notFound), true, false},
		{"wrapped 409", fmt.Errorf("activate failed: %w", conflict), false, true},
		{"wrapped other status", fmt.Errorf("nope: %w", forbidden), false, false},
		{"wrapped non-API error", fmt.Errorf("nope: %w", plain), false, false},
		{"plain error", plain, false, false},
		{"nil", nil, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsNotFound(tt.err); got != tt.wantNotFound {
				t.Errorf("IsNotFound() = %v, want %v", got, tt.wantNotFound)
			}
			if got := IsConflict(tt.err); got != tt.wantConflict {
				t.Errorf("IsConflict() = %v, want %v", got, tt.wantConflict)
			}
		})
	}
}
