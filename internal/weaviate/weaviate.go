// Package weaviate provides typed wrappers over the Weaviate v1 REST
// endpoints that weavectl manages: collections (schema), authz roles, db
// users, aliases, tenants, shards, cluster status, and backups.
package weaviate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sriharip316/weavectl/internal/client"
)

// Collections and roles are stored as generic maps: Weaviate normalizes
// definitions heavily on read (fills in every default), and apply logic
// must diff user intent against that normalized shape without this CLI
// modeling every field.

// doGet performs a GET; canceling ctx aborts the request.
func doGet(ctx context.Context, c *client.Client, path string) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, path, nil)
}

// ---- Collections (schema) ----

type Collection = map[string]any

// ListCollections returns the classes from GET /v1/schema.
func ListCollections(ctx context.Context, c *client.Client) ([]Collection, error) {
	raw, err := doGet(ctx, c, "/schema")
	if err != nil {
		return nil, err
	}
	var out struct {
		Classes []Collection `json:"classes"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse collections: %w", err)
	}
	return out.Classes, nil
}

// GetCollection fetches a single collection definition.
func GetCollection(ctx context.Context, c *client.Client, name string) (Collection, error) {
	if strings.TrimSpace(name) == "" {
		return nil, &client.APIError{StatusCode: http.StatusNotFound}
	}
	raw, err := doGet(ctx, c, "/schema/"+url.PathEscape(name))
	if err != nil {
		return nil, err
	}
	var out Collection
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse collection: %w", err)
	}
	return out, nil
}

func CreateCollection(ctx context.Context, c *client.Client, def Collection) (Collection, error) {
	raw, err := c.Do(ctx, http.MethodPost, "/schema", def)
	if err != nil {
		return nil, err
	}
	var out Collection
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse collection: %w", err)
	}
	return out, nil
}

// UpdateCollection performs the mutable-config PUT. The body must be the
// full class definition including all existing properties (the server
// rejects partial bodies); callers are responsible for assembling it.
func UpdateCollection(ctx context.Context, c *client.Client, name string, def Collection) (Collection, error) {
	if strings.TrimSpace(name) == "" {
		return nil, &client.APIError{StatusCode: http.StatusNotFound}
	}
	raw, err := c.Do(ctx, http.MethodPut, "/schema/"+url.PathEscape(name), def)
	if err != nil {
		return nil, err
	}
	var out Collection
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse collection: %w", err)
	}
	return out, nil
}

func DeleteCollection(ctx context.Context, c *client.Client, name string) error {
	if strings.TrimSpace(name) == "" {
		return &client.APIError{StatusCode: http.StatusNotFound}
	}
	_, err := c.Do(ctx, http.MethodDelete, "/schema/"+url.PathEscape(name), nil)
	return err
}

// AddProperty attaches a new property to a collection.
func AddProperty(ctx context.Context, c *client.Client, collection string, prop map[string]any) error {
	_, err := c.Do(ctx, http.MethodPost, "/schema/"+url.PathEscape(collection)+"/properties", prop)
	return err
}

// ---- Shards ----

type ShardStatus struct {
	Name            string         `json:"name"`
	Status          string         `json:"status"`
	PerNodeStatus   map[string]any `json:"per_node_status"`
	VectorQueueSize int            `json:"vectorQueueSize"`
}

func ListShards(ctx context.Context, c *client.Client, collection string) ([]ShardStatus, error) {
	raw, err := doGet(ctx, c, "/schema/"+url.PathEscape(collection)+"/shards")
	if err != nil {
		return nil, err
	}
	var out []ShardStatus
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse shards: %w", err)
	}
	return out, nil
}

// ---- Tenants ----

type Tenant struct {
	Name           string `json:"name"`
	ActivityStatus string `json:"activityStatus"`
}

func ListTenants(ctx context.Context, c *client.Client, collection string) ([]Tenant, error) {
	raw, err := doGet(ctx, c, "/schema/"+url.PathEscape(collection)+"/tenants")
	if err != nil {
		return nil, err
	}
	var out []Tenant
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse tenants: %w", err)
	}
	return out, nil
}

// CreateTenants creates the given tenants in one request.
func CreateTenants(ctx context.Context, c *client.Client, collection string, tenants []Tenant) error {
	_, err := c.Do(ctx, http.MethodPost, "/schema/"+url.PathEscape(collection)+"/tenants", tenants)
	return err
}

// UpdateTenants updates activity status for the given tenants.
func UpdateTenants(ctx context.Context, c *client.Client, collection string, tenants []Tenant) error {
	_, err := c.Do(ctx, http.MethodPut, "/schema/"+url.PathEscape(collection)+"/tenants", tenants)
	return err
}

// DeleteTenants removes tenants by name.
func DeleteTenants(ctx context.Context, c *client.Client, collection string, names []string) error {
	_, err := c.Do(ctx, http.MethodDelete, "/schema/"+url.PathEscape(collection)+"/tenants", names)
	return err
}

// ---- Aliases ----

type Alias struct {
	Alias string `json:"alias"`
	Class string `json:"class"`
}

func ListAliases(ctx context.Context, c *client.Client) ([]Alias, error) {
	raw, err := doGet(ctx, c, "/aliases")
	if err != nil {
		return nil, err
	}
	var out struct {
		Aliases []Alias `json:"aliases"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse aliases: %w", err)
	}
	return out.Aliases, nil
}

func CreateAlias(ctx context.Context, c *client.Client, a Alias) error {
	_, err := c.Do(ctx, http.MethodPost, "/aliases", a)
	return err
}

// UpdateAlias retargets an existing alias to another collection.
func UpdateAlias(ctx context.Context, c *client.Client, alias, class string) error {
	_, err := c.Do(ctx, http.MethodPut, "/aliases/"+url.PathEscape(alias), map[string]string{"class": class})
	return err
}

func DeleteAlias(ctx context.Context, c *client.Client, alias string) error {
	if strings.TrimSpace(alias) == "" {
		return &client.APIError{StatusCode: http.StatusNotFound}
	}
	_, err := c.Do(ctx, http.MethodDelete, "/aliases/"+url.PathEscape(alias), nil)
	return err
}

// ---- Roles (authz) ----

type Role struct {
	Name        string           `json:"name"`
	Permissions []map[string]any `json:"permissions"`
}

func ListRoles(ctx context.Context, c *client.Client) ([]Role, error) {
	raw, err := doGet(ctx, c, "/authz/roles")
	if err != nil {
		return nil, err
	}
	var out []Role
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse roles: %w", err)
	}
	return out, nil
}

func GetRole(ctx context.Context, c *client.Client, name string) (*Role, error) {
	if strings.TrimSpace(name) == "" {
		return nil, &client.APIError{StatusCode: http.StatusNotFound}
	}
	raw, err := doGet(ctx, c, "/authz/roles/"+url.PathEscape(name))
	if err != nil {
		return nil, err
	}
	var out Role
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse role: %w", err)
	}
	return &out, nil
}

func CreateRole(ctx context.Context, c *client.Client, r Role) error {
	_, err := c.Do(ctx, http.MethodPost, "/authz/roles", r)
	return err
}

func DeleteRole(ctx context.Context, c *client.Client, name string) error {
	if strings.TrimSpace(name) == "" {
		return &client.APIError{StatusCode: http.StatusNotFound}
	}
	_, err := c.Do(ctx, http.MethodDelete, "/authz/roles/"+url.PathEscape(name), nil)
	return err
}

func AddPermissions(ctx context.Context, c *client.Client, role string, perms []map[string]any) error {
	_, err := c.Do(ctx, http.MethodPost, "/authz/roles/"+url.PathEscape(role)+"/add-permissions",
		map[string]any{"permissions": perms})
	return err
}

func RemovePermissions(ctx context.Context, c *client.Client, role string, perms []map[string]any) error {
	_, err := c.Do(ctx, http.MethodPost, "/authz/roles/"+url.PathEscape(role)+"/remove-permissions",
		map[string]any{"permissions": perms})
	return err
}

// ---- Users (db users) ----

type DBUser struct {
	UserID             string   `json:"userId"`
	DBUserType         string   `json:"dbUserType"`
	Roles              []string `json:"roles"`
	Active             bool     `json:"active"`
	CreatedAt          string   `json:"createdAt"`
	APIKeyFirstLetters string   `json:"apiKeyFirstLetters"`
	LastUsedAt         string   `json:"lastUsedAt"`
}

func ListUsers(ctx context.Context, c *client.Client) ([]DBUser, error) {
	raw, err := doGet(ctx, c, "/users/db?includeLastUsedTime=true")
	if err != nil {
		return nil, err
	}
	var out []DBUser
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse users: %w", err)
	}
	return out, nil
}

func GetUser(ctx context.Context, c *client.Client, id string) (*DBUser, error) {
	if strings.TrimSpace(id) == "" {
		return nil, &client.APIError{StatusCode: http.StatusNotFound}
	}
	raw, err := doGet(ctx, c, "/users/db/"+url.PathEscape(id)+"?includeLastUsedTime=true")
	if err != nil {
		return nil, err
	}
	var out DBUser
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse user: %w", err)
	}
	return &out, nil
}

// CreateUser creates a db user; the generated API key is returned exactly
// once and cannot be retrieved again later.
func CreateUser(ctx context.Context, c *client.Client, id string) (apiKey string, err error) {
	raw, err := c.Do(ctx, http.MethodPost, "/users/db/"+url.PathEscape(id), map[string]any{})
	if err != nil {
		return "", err
	}
	var out struct {
		APIKey string `json:"apikey"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse api key response: %w", err)
	}
	return out.APIKey, nil
}

// RotateUserKey revokes a db user's current API key and returns the newly
// generated one exactly once; the old key stops authenticating immediately.
func RotateUserKey(ctx context.Context, c *client.Client, id string) (apiKey string, err error) {
	if strings.TrimSpace(id) == "" {
		return "", &client.APIError{StatusCode: http.StatusNotFound}
	}
	raw, err := c.Do(ctx, http.MethodPost, "/users/db/"+url.PathEscape(id)+"/rotate-key", map[string]any{})
	if err != nil {
		return "", err
	}
	var out struct {
		APIKey string `json:"apikey"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse api key response: %w", err)
	}
	return out.APIKey, nil
}

func DeleteUser(ctx context.Context, c *client.Client, id string) error {
	if strings.TrimSpace(id) == "" {
		return &client.APIError{StatusCode: http.StatusNotFound}
	}
	_, err := c.Do(ctx, http.MethodDelete, "/users/db/"+url.PathEscape(id), nil)
	return err
}

// ActivateUser re-enables a deactivated db user; DeactivateUser disables
// one while keeping its API key. The server answers 409 when the user is
// already in the requested state, which apply treats as "nothing to do".
func ActivateUser(ctx context.Context, c *client.Client, id string) error {
	if strings.TrimSpace(id) == "" {
		return &client.APIError{StatusCode: http.StatusNotFound}
	}
	_, err := c.Do(ctx, http.MethodPost, "/users/db/"+url.PathEscape(id)+"/activate", nil)
	return err
}

func DeactivateUser(ctx context.Context, c *client.Client, id string) error {
	if strings.TrimSpace(id) == "" {
		return &client.APIError{StatusCode: http.StatusNotFound}
	}
	_, err := c.Do(ctx, http.MethodPost, "/users/db/"+url.PathEscape(id)+"/deactivate", nil)
	return err
}

// AssignRoles grants roles to a user; RevokeRoles removes them.
func AssignRoles(ctx context.Context, c *client.Client, userID string, roles []string) error {
	_, err := c.Do(ctx, http.MethodPost, "/authz/users/"+url.PathEscape(userID)+"/assign",
		map[string]any{"roles": roles, "userType": "db"})
	return err
}

func RevokeRoles(ctx context.Context, c *client.Client, userID string, roles []string) error {
	_, err := c.Do(ctx, http.MethodPost, "/authz/users/"+url.PathEscape(userID)+"/revoke",
		map[string]any{"roles": roles, "userType": "db"})
	return err
}

// ---- Cluster / meta ----

type NodeStatus struct {
	Name            string `json:"name"`
	Status          string `json:"status"`
	Version         string `json:"version"`
	GitHash         string `json:"gitHash"`
	Shards          any    `json:"shards"`
	OperationalMode string `json:"operationalMode"`
}

func ListNodes(ctx context.Context, c *client.Client) ([]NodeStatus, error) {
	raw, err := doGet(ctx, c, "/nodes")
	if err != nil {
		return nil, err
	}
	var out struct {
		Nodes []NodeStatus `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse nodes: %w", err)
	}
	return out.Nodes, nil
}

type Meta struct {
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
}

func GetMeta(ctx context.Context, c *client.Client) (*Meta, error) {
	raw, err := doGet(ctx, c, "/meta")
	if err != nil {
		return nil, err
	}
	var out Meta
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse meta: %w", err)
	}
	return &out, nil
}

// CapitalizeName mirrors Weaviate's server-side name normalization for
// collections and aliases: the first character is upper-cased ("my_class"
// is stored as "My_class"). Applying it client-side means users can refer
// to objects with lower-case names the same way they were created. The
// first character is upper-cased per Unicode simple case mapping, so
// multi-byte runes are handled ("über" becomes "Über"); runes without an
// uppercase form (e.g. CJK) and names not starting with a letter are
// returned unchanged. Names that are not valid UTF-8 are left untouched.
func CapitalizeName(name string) string {
	if name == "" {
		return name
	}
	r, size := utf8.DecodeRuneInString(name)
	if r == utf8.RuneError && size == 1 {
		return name // invalid UTF-8: pass through instead of corrupting
	}
	return string(unicode.ToUpper(r)) + name[size:]
}

// ResolveCollection finds an existing collection by name. Exact (and
// first-letter-capitalized) matches win; otherwise a unique case-insensitive
// match is accepted so "mtnews" finds "MTNews". The returned name is the
// collection's canonical spelling.
func ResolveCollection(ctx context.Context, c *client.Client, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", &client.APIError{StatusCode: http.StatusNotFound}
	}
	try := func(n string) (bool, error) {
		_, err := GetCollection(ctx, c, n)
		if err == nil {
			return true, nil
		}
		if client.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, n := range []string{CapitalizeName(name), name} {
		ok, err := try(n)
		if err != nil {
			return "", err
		}
		if ok {
			return n, nil
		}
	}
	list, err := ListCollections(ctx, c)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, col := range list {
		if n, _ := col["class"].(string); strings.EqualFold(n, name) {
			matches = append(matches, n)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", &client.APIError{StatusCode: http.StatusNotFound}
	default:
		return "", fmt.Errorf("ambiguous collection name %q matches %v", name, matches)
	}
}

// ResolveAlias finds an existing alias by name with the same matching
// rules as ResolveCollection.
func ResolveAlias(ctx context.Context, c *client.Client, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", &client.APIError{StatusCode: http.StatusNotFound}
	}
	list, err := ListAliases(ctx, c)
	if err != nil {
		return "", err
	}
	cap := CapitalizeName(name)
	var matches []string
	for _, a := range list {
		if a.Alias == cap || a.Alias == name {
			return a.Alias, nil
		}
		if strings.EqualFold(a.Alias, name) {
			matches = append(matches, a.Alias)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", &client.APIError{StatusCode: http.StatusNotFound}
	default:
		return "", fmt.Errorf("ambiguous alias name %q matches %v", name, matches)
	}
}
