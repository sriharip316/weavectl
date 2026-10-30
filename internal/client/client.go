// Package client provides a thin HTTP client for the Weaviate v1 REST API.
// It handles authentication, JSON transport, and normalizing Weaviate's
// varied error response shapes into a single error type.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// APIError carries a non-2xx response from Weaviate.
type APIError struct {
	StatusCode int
	// Messages holds every human-readable message found in the error body.
	Messages []string
	// Op names the operation, e.g. `GET /v1/schema/Article`.
	Op string
}

func (e *APIError) Error() string {
	if len(e.Messages) == 0 {
		return fmt.Sprintf("the server responded with status %d (%s)", e.StatusCode, http.StatusText(e.StatusCode))
	}
	return fmt.Sprintf("the server responded with status %d (%s): %s",
		e.StatusCode, http.StatusText(e.StatusCode), strings.Join(e.Messages, "; "))
}

// Reason returns a kubectl-style short reason for the failure.
func (e *APIError) Reason() string {
	switch e.StatusCode {
	case http.StatusNotFound:
		return "NotFound"
	case http.StatusUnauthorized:
		return "Unauthorized"
	case http.StatusForbidden:
		return "Forbidden"
	case http.StatusConflict:
		return "AlreadyExists"
	case http.StatusUnprocessableEntity:
		return "Invalid"
	case http.StatusTooManyRequests:
		return "TooManyRequests"
	default:
		return fmt.Sprintf("ServerError(%d)", e.StatusCode)
	}
}

// IsNotFound reports whether err is a 404 from the server, including
// when it has been wrapped with %w along the way.
func IsNotFound(err error) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	return ok && apiErr.StatusCode == http.StatusNotFound
}

// IsConflict reports whether err is a 409 from the server, including
// when it has been wrapped with %w along the way.
func IsConflict(err error) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	return ok && apiErr.StatusCode == http.StatusConflict
}

// Client talks to a single Weaviate instance.
type Client struct {
	Server  string // base URL, e.g. http://localhost:8080
	APIKey  string // sent as an Authorization: Bearer credential
	HTTP    *http.Client
	baseURL string // Server without a trailing slash
}

func New(server, apiKey string) *Client {
	s := strings.TrimRight(server, "/")
	return &Client{
		Server:  s,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		baseURL: s,
	}
}

// Do performs a request against the v1 API. path is relative, e.g. "/schema".
// body may be nil or any JSON-marshalable value. The raw response body is
// returned for 2xx responses; anything else becomes an *APIError.
func (c *Client) Do(method, path string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequest(method, c.baseURL+"/v1"+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 300 {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Messages:   parseErrorBody(raw),
			Op:         fmt.Sprintf("%s %s", method, path),
		}
	}
	return json.RawMessage(raw), nil
}

// Weaviate error bodies vary by handler; the observed shapes are:
//
//	{"error":[{"message":"..."}]}              most handlers
//	{"allowed":null,"error":[{"message":".."}]} schema updates
//	{"code":401,"message":"..."}               auth middleware
//
// 404 responses frequently have empty bodies, so an empty Messages slice is
// normal and callers fall back to the status text.
func parseErrorBody(raw []byte) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	var generic struct {
		Error []struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &generic); err == nil && len(generic.Error) > 0 {
		msgs := make([]string, 0, len(generic.Error))
		for _, m := range generic.Error {
			if m.Message != "" {
				msgs = append(msgs, m.Message)
			}
		}
		if len(msgs) > 0 {
			return msgs
		}
	}

	var flat struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &flat); err == nil && flat.Message != "" {
		return []string{flat.Message}
	}
	return nil
}
