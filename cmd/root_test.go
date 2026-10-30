package cmd

import (
	"errors"
	"net/http"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

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

func TestSignalHandler_NormalExitNoLeak(t *testing.T) {
	initial := runtime.NumGoroutine()
	sigCh := make(chan os.Signal, 1)
	_, cleanup := setupSignalHandlerWith(sigCh, func(code int) {})
	cleanup()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= initial {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runtime.NumGoroutine() > initial {
		t.Fatalf("leaked goroutine: before=%d, after=%d", initial, runtime.NumGoroutine())
	}
}

func TestSignalHandler_FirstSignalCancelsContext(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	exitCalled := false
	ctx, cleanup := setupSignalHandlerWith(sigCh, func(code int) {
		exitCalled = true
	})
	defer cleanup()

	sigCh <- syscall.SIGINT

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("expected context to be canceled on first signal")
	}

	if exitCalled {
		t.Fatal("first signal must not call exit")
	}
}

func TestSignalHandler_SecondSignalTriggersHardExit(t *testing.T) {
	sigCh := make(chan os.Signal, 2)
	exitCode := -1
	exitCh := make(chan int, 1)
	ctx, cleanup := setupSignalHandlerWith(sigCh, func(code int) {
		exitCode = code
		exitCh <- code
	})
	defer cleanup()

	sigCh <- syscall.SIGINT
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("expected context to be canceled on first signal")
	}

	sigCh <- syscall.SIGINT
	select {
	case code := <-exitCh:
		if code != 130 {
			t.Fatalf("expected exit code 130, got %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("expected second signal to invoke exit(130)")
	}

	if exitCode != 130 {
		t.Fatalf("expected exit code 130, got %d", exitCode)
	}
}

func TestSignalHandler_CleanupAfterFirstSignal(t *testing.T) {
	initial := runtime.NumGoroutine()
	sigCh := make(chan os.Signal, 1)
	ctx, cleanup := setupSignalHandlerWith(sigCh, func(code int) {})

	sigCh <- syscall.SIGINT
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("expected context to be canceled")
	}

	cleanup()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= initial {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runtime.NumGoroutine() > initial {
		t.Fatalf("leaked goroutine after cleanup post-signal: before=%d, after=%d", initial, runtime.NumGoroutine())
	}
}
