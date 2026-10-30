// Package apply implements create-or-update ("kubectl apply") semantics for
// each Weaviate object kind. A single apply may fan out into several REST
// calls — for example a collection update is one PUT for mutable settings
// plus one POST per new property, and a role update is an add/remove
// permission diff — all handled here so the caller just gets a Result.
package apply

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/sriharip316/weavectl/internal/client"
	"github.com/sriharip316/weavectl/internal/manifest"
	"github.com/sriharip316/weavectl/internal/weaviate"
)

// Result reports what one apply did.
type Result struct {
	Kind   manifest.Kind
	Name   string
	Action string   // "created", "configured", or "unchanged"
	Info   []string // extra human-readable notes (new API key, warnings, …)
}

// Apply converges each document toward its desired state, in order.
func Apply(c *client.Client, docs []manifest.Document) ([]Result, error) {
	var results []Result
	for _, doc := range docs {
		var (
			r   Result
			err error
		)
		switch doc.Kind {
		case manifest.KindCollection:
			r, err = collection(c, doc)
		case manifest.KindRole:
			r, err = role(c, doc)
		case manifest.KindUser:
			r, err = user(c, doc)
		case manifest.KindAlias:
			r, err = alias(c, doc)
		case manifest.KindTenants:
			r, err = tenants(c, doc)
		case manifest.KindBackup:
			r, err = backup(c, doc)
		case manifest.KindRestore:
			r, err = restore(c, doc)
		default:
			err = fmt.Errorf("cannot apply a %q object; add a kind field to the manifest", doc.Kind)
		}
		if err != nil {
			return results, fmt.Errorf("%s %q: %w", doc.Kind, doc.Name, err)
		}
		r.Kind = doc.Kind
		if r.Name == "" {
			r.Name = doc.Name
		}
		results = append(results, r)
	}
	return results, nil
}

// ---- collections ----

func collection(c *client.Client, doc manifest.Document) (Result, error) {
	desired := doc.Object
	name := weaviate.CapitalizeName(doc.Name)

	resolved, err := weaviate.ResolveCollection(c, doc.Name)
	if err == nil {
		name = resolved
	} else if !client.IsNotFound(err) {
		return Result{}, err
	}

	current, err := weaviate.GetCollection(c, name)
	if client.IsNotFound(err) {
		if _, err := weaviate.CreateCollection(c, desired); err != nil {
			return Result{}, err
		}
		return Result{Name: name, Action: "created"}, nil
	}
	if err != nil {
		return Result{}, err
	}

	res := Result{Name: name, Action: "configured"}

	// PUT requires the full definition with every existing property present;
	// property entries may only differ from the stored ones in description.
	// New properties go through POST .../properties afterwards.
	currentPropList, _ := current["properties"].([]any)
	currentProps := propertiesOf(current)
	desiredProps := propertiesOf(desired)

	putProps := make([]any, 0, len(currentPropList))
	putProps = append(putProps, currentPropList...) // server definitions, order preserved

	var newProps []map[string]any
	for _, dp := range desiredProps {
		cp, exists := currentProps[propertyName(dp)]
		if !exists {
			newProps = append(newProps, dp)
			continue
		}
		// Existing property: only description can change. Warn about other
		// explicit differences so silent no-ops are visible.
		for k, dv := range dp {
			if k == "name" || k == "description" {
				continue
			}
			if cv, ok := cp[k]; !ok || !leafEqual(cv, dv) {
				res.Info = append(res.Info,
					fmt.Sprintf("property %q: %s is immutable and was not changed (recreate the collection to change it)",
						propertyName(dp), k))
				break
			}
		}
		if desc, ok := dp["description"]; ok {
			merged := map[string]any{}
			maps.Copy(merged, cp)
			merged["description"] = desc
			// replace in putProps
			for i, p := range putProps {
				if pm, ok := p.(map[string]any); ok && propertyName(pm) == propertyName(dp) {
					putProps[i] = merged
				}
			}
		}
	}

	body := deepMerge(current, desired)
	body["class"] = name
	body["properties"] = putProps

	// Go's json.Marshal sorts map keys, so canonical bytes comparison works.
	configChanged := !bytesEqualJSON(body, current)

	if _, err := weaviate.UpdateCollection(c, name, body); err != nil {
		return res, err
	}

	for _, np := range newProps {
		if err := weaviate.AddProperty(c, name, np); err != nil {
			return res, fmt.Errorf("add property %q: %w", propertyName(np), err)
		}
		res.Info = append(res.Info, fmt.Sprintf("property %q added", propertyName(np)))
	}
	if !configChanged && len(newProps) == 0 && len(res.Info) == 0 {
		res.Action = "unchanged"
	}
	return res, nil
}

func bytesEqualJSON(a, b map[string]any) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func propertiesOf(def map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	raw, ok := def["properties"].([]any)
	if !ok {
		return out
	}
	for _, p := range raw {
		if pm, ok := p.(map[string]any); ok {
			if n := propertyName(pm); n != "" {
				out[n] = pm
			}
		}
	}
	return out
}

func propertyName(p map[string]any) string {
	s, _ := p["name"].(string)
	return s
}

// deepMerge returns base with overlay applied: maps merge recursively,
// everything else from the overlay replaces the base value.
func deepMerge(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overlay))
	maps.Copy(out, base)
	for k, ov := range overlay {
		if bv, ok := out[k].(map[string]any); ok {
			if om, ok := ov.(map[string]any); ok {
				out[k] = deepMerge(bv, om)
				continue
			}
		}
		out[k] = ov
	}
	return out
}

func leafEqual(a, b any) bool {
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// ---- roles ----

// domainFor maps a permission action to its resource object key and the
// default leaf values the server fills in when omitted. Defaults matter
// only to recognize server-filled leaves when diffing; they are never sent.
var permissionDomains = map[string]struct {
	key      string
	defaults map[string]any
}{
	"create_data": {"data", map[string]any{"object": "*", "tenant": "*"}},
	"read_data":   {"data", map[string]any{"object": "*", "tenant": "*"}},
	"update_data": {"data", map[string]any{"object": "*", "tenant": "*"}},
	"delete_data": {"data", map[string]any{"object": "*", "tenant": "*"}},

	"create_collections": {"collections", nil},
	"read_collections":   {"collections", nil},
	"update_collections": {"collections", nil},
	"delete_collections": {"collections", nil},

	"create_tenants": {"tenants", map[string]any{"tenant": "*"}},
	"read_tenants":   {"tenants", map[string]any{"tenant": "*"}},
	"update_tenants": {"tenants", map[string]any{"tenant": "*"}},
	"delete_tenants": {"tenants", map[string]any{"tenant": "*"}},

	"manage_backups": {"backups", nil},
	"read_backups":   {"backups", nil},

	"read_nodes": {"nodes", map[string]any{"verbosity": "minimal"}},

	"create_roles": {"roles", map[string]any{"scope": "match"}},
	"read_roles":   {"roles", map[string]any{"scope": "match"}},
	"update_roles": {"roles", map[string]any{"scope": "match"}},
	"delete_roles": {"roles", map[string]any{"scope": "match"}},

	"assign_and_revoke_users": {"users", nil},
	"create_users":            {"users", nil},
	"read_users":              {"users", nil},
	"update_users":            {"users", nil},
	"delete_users":            {"users", nil},

	"assign_and_revoke_groups": {"groups", map[string]any{"groupType": "oidc"}},
	"read_groups":              {"groups", map[string]any{"groupType": "oidc"}},

	"create_replicate": {"replicate", map[string]any{"shard": "*"}},
	"read_replicate":   {"replicate", map[string]any{"shard": "*"}},
	"update_replicate": {"replicate", map[string]any{"shard": "*"}},
	"delete_replicate": {"replicate", map[string]any{"shard": "*"}},

	"create_aliases": {"aliases", map[string]any{"collection": "*"}},
	"read_aliases":   {"aliases", map[string]any{"collection": "*"}},
	"update_aliases": {"aliases", map[string]any{"collection": "*"}},
	"delete_aliases": {"aliases", map[string]any{"collection": "*"}},

	"manage_namespaces": {"namespaces", nil},
}

// nameFields are resource leaves holding object names that Weaviate
// capitalizes server-side; mirroring that here keeps diffs convergent.
var nameFields = map[string]bool{"collection": true, "alias": true}

func role(c *client.Client, doc manifest.Document) (Result, error) {
	desired := &weaviate.Role{Name: doc.Name}
	if rawPerms, ok := doc.Object["permissions"].([]any); ok {
		for _, p := range rawPerms {
			if pm, ok := p.(map[string]any); ok {
				desired.Permissions = append(desired.Permissions, pm)
			}
		}
	}

	current, err := weaviate.GetRole(c, desired.Name)
	if client.IsNotFound(err) {
		if err := weaviate.CreateRole(c, *desired); err != nil {
			return Result{}, err
		}
		return Result{Name: desired.Name, Action: "created"}, nil
	}
	if err != nil {
		return Result{}, err
	}

	currentPerms := permList(current.Permissions)
	desiredPerms := make([]map[string]any, 0, len(desired.Permissions))
	for _, p := range desired.Permissions {
		// Compare and send in "effective space": server-filled defaults
		// (and explicit values equal to them) are stripped, so an omitted
		// field and an explicit "*" are treated identically.
		desiredPerms = append(desiredPerms, effectivePerm(normalizePerm(p)))
	}

	var toAdd, toRemove []map[string]any
	for _, dp := range desiredPerms {
		if !coveredBy(dp, currentPerms) {
			toAdd = append(toAdd, dp)
		}
	}
	for _, cp := range currentPerms {
		// The stored form carries server-filled defaults; strip them so the
		// comparison works against the user's raw intent.
		eff := effectivePerm(cp)
		if !coveredBy(eff, desiredPerms) {
			toRemove = append(toRemove, cp)
		}
	}

	res := Result{Name: desired.Name, Action: "unchanged"}
	if len(toAdd) > 0 || len(toRemove) > 0 {
		res.Action = "configured"
		if len(toAdd) > 0 {
			if err := weaviate.AddPermissions(c, desired.Name, toAdd); err != nil {
				return res, fmt.Errorf("add permissions: %w", err)
			}
			res.Info = append(res.Info, fmt.Sprintf("added %d permission(s)", len(toAdd)))
		}
		if len(toRemove) > 0 {
			// Removing uses the server's stored form so the server can match it.
			if err := weaviate.RemovePermissions(c, desired.Name, toRemove); err != nil {
				return res, fmt.Errorf("remove permissions: %w", err)
			}
			res.Info = append(res.Info, fmt.Sprintf("removed %d permission(s)", len(toRemove)))
		}
	}
	return res, nil
}

func permList(raw []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		out = append(out, normalizePerm(p))
	}
	return out
}

// normalizePerm capitalizes name-like resource values, mirroring the server.
func normalizePerm(p map[string]any) map[string]any {
	out := map[string]any{"action": p["action"]}
	for k, v := range p {
		if k == "action" {
			continue
		}
		if res, ok := v.(map[string]any); ok {
			nr := map[string]any{}
			for rk, rv := range res {
				if nameFields[rk] {
					if s, ok := rv.(string); ok {
						rv = weaviate.CapitalizeName(s)
					}
				}
				nr[rk] = rv
			}
			out[k] = nr
		} else {
			out[k] = v
		}
	}
	return out
}

// effectivePerm strips server-filled default leaves from a stored permission
// so it can be compared with the user's raw intent.
func effectivePerm(p map[string]any) map[string]any {
	action, _ := p["action"].(string)
	domain, known := permissionDomains[action]
	out := map[string]any{"action": action}
	for k, v := range p {
		if k == "action" || (known && k == domain.key) {
			continue
		}
		out[k] = v
	}
	if !known {
		return out
	}
	res, ok := v_orNil(p, domain.key)
	if !ok {
		return out
	}
	effective := map[string]any{}
	for rk, rv := range res {
		if dv, isDefault := domain.defaults[rk]; isDefault && leafEqual(rv, dv) {
			continue
		}
		effective[rk] = rv
	}
	if len(effective) > 0 {
		out[domain.key] = effective
	}
	return out
}

func v_orNil(p map[string]any, key string) (map[string]any, bool) {
	v, ok := p[key]
	if !ok {
		return nil, false
	}
	res, ok := v.(map[string]any)
	return res, ok
}

// coveredBy reports whether perm is already granted by some permission in
// set: same action and the candidate's effective (default-stripped) form
// matches perm leaf-for-leaf.
func coveredBy(perm map[string]any, set []map[string]any) bool {
	action, _ := perm["action"].(string)
	for _, cand := range set {
		if cand["action"] != action {
			continue
		}
		eff := effectivePerm(cand)
		if permEqual(perm, eff) {
			return true
		}
	}
	return false
}

// permEqual: a and b carry the same top-level keys and, for resource
// objects, the same leaves with equal values. Both sides are expected in
// effective space (server-filled defaults already stripped), so anything
// extra on either side — a resource object or a leaf — is a real scope
// difference, not a default: an unrestricted permission never matches a
// restricted one in either direction.
func permEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		am, aIsMap := av.(map[string]any)
		bm, bIsMap := bv.(map[string]any)
		if aIsMap != bIsMap {
			return false
		}
		if aIsMap {
			if len(am) != len(bm) {
				return false
			}
			for lk, lv := range am {
				if !leafEqual(bm[lk], lv) {
					return false
				}
			}
			continue
		}
		if !leafEqual(av, bv) {
			return false
		}
	}
	return true
}

// ---- users ----

func user(c *client.Client, doc manifest.Document) (Result, error) {
	userID := doc.Name
	var desiredRoles []string
	if raw, ok := doc.Object["roles"].([]any); ok {
		for _, r := range raw {
			if s, ok := r.(string); ok {
				desiredRoles = append(desiredRoles, s)
			}
		}
	}
	// Like roles, "active" is converged only when the manifest carries the
	// key; omitted leaves the user's current state alone.
	desiredActive, hasActive := false, false
	if raw, ok := doc.Object["active"]; ok {
		b, ok := raw.(bool)
		if !ok {
			return Result{}, fmt.Errorf(`"active" must be true or false, got %v`, raw)
		}
		desiredActive, hasActive = b, true
	}

	current, err := weaviate.GetUser(c, userID)
	res := Result{Name: userID}
	if client.IsNotFound(err) {
		apiKey, err := weaviate.CreateUser(c, userID)
		if err != nil {
			return res, err
		}
		res.Action = "created"
		res.Info = append(res.Info,
			"API key (shown once, store it now): "+apiKey)
		// CreateUser always yields an active user.
		if hasActive && !desiredActive {
			if err := weaviate.DeactivateUser(c, userID); err != nil {
				return res, fmt.Errorf("deactivate user: %w", err)
			}
			res.Info = append(res.Info, "deactivated user")
		}
	} else if err != nil {
		return res, err
	} else {
		res.Action = "unchanged"
		if _, hasRoles := doc.Object["roles"]; hasRoles {
			toAssign, toRevoke := diffStrings(current.Roles, desiredRoles)
			if len(toAssign) > 0 {
				if err := weaviate.AssignRoles(c, userID, toAssign); err != nil {
					return res, fmt.Errorf("assign roles: %w", err)
				}
				res.Info = append(res.Info, "assigned role(s): "+strings.Join(toAssign, ", "))
			}
			if len(toRevoke) > 0 {
				if err := weaviate.RevokeRoles(c, userID, toRevoke); err != nil {
					return res, fmt.Errorf("revoke roles: %w", err)
				}
				res.Info = append(res.Info, "revoked role(s): "+strings.Join(toRevoke, ", "))
			}
			if len(toAssign)+len(toRevoke) > 0 {
				res.Action = "configured"
			}
		}
		if hasActive && current.Active != desiredActive {
			// A 409 means the state flipped since the read; the desired
			// state already holds, so treat it as success.
			if desiredActive {
				if err := weaviate.ActivateUser(c, userID); err != nil && !client.IsConflict(err) {
					return res, fmt.Errorf("activate user: %w", err)
				}
				res.Info = append(res.Info, "activated user")
			} else {
				if err := weaviate.DeactivateUser(c, userID); err != nil && !client.IsConflict(err) {
					return res, fmt.Errorf("deactivate user: %w", err)
				}
				res.Info = append(res.Info, "deactivated user")
			}
			res.Action = "configured"
		}
	}

	// A newly created user may still need its roles assigned.
	if res.Action == "created" && len(desiredRoles) > 0 {
		if err := weaviate.AssignRoles(c, userID, desiredRoles); err != nil {
			return res, fmt.Errorf("assign roles: %w", err)
		}
		res.Info = append(res.Info, "assigned role(s): "+strings.Join(desiredRoles, ", "))
	}
	return res, nil
}

func diffStrings(current, desired []string) (toAdd, toRemove []string) {
	set := func(xs []string) map[string]bool {
		m := map[string]bool{}
		for _, x := range xs {
			m[x] = true
		}
		return m
	}
	cd, dc := set(current), set(desired)
	for _, d := range desired {
		if !cd[d] {
			toAdd = append(toAdd, d)
		}
	}
	for _, c := range current {
		if !dc[c] {
			toRemove = append(toRemove, c)
		}
	}
	return
}

// ---- aliases ----

func alias(c *client.Client, doc manifest.Document) (Result, error) {
	name := weaviate.CapitalizeName(doc.Name)
	target, _ := doc.Object["class"].(string)
	if resolved, err := weaviate.ResolveCollection(c, target); err == nil {
		target = resolved
	} else if !client.IsNotFound(err) {
		return Result{}, err
	} else {
		target = weaviate.CapitalizeName(target)
	}

	current, err := weaviate.ListAliases(c)
	if err != nil {
		return Result{}, err
	}
	for _, a := range current {
		if strings.EqualFold(a.Alias, name) {
			name = a.Alias
			if a.Class == target {
				return Result{Name: name, Action: "unchanged"}, nil
			}
			if err := weaviate.UpdateAlias(c, name, target); err != nil {
				return Result{}, err
			}
			return Result{Name: name, Action: "configured",
				Info: []string{fmt.Sprintf("retargeted %s: %s -> %s", name, a.Class, target)}}, nil
		}
	}
	if err := weaviate.CreateAlias(c, weaviate.Alias{Alias: name, Class: target}); err != nil {
		return Result{}, err
	}
	return Result{Name: name, Action: "created"}, nil
}

// ---- tenants ----

// tenantStatusEquiv maps request-time statuses to the temperature-style
// names the server returns (ACTIVE reads back as HOT, INACTIVE as COLD).
var tenantStatusEquiv = map[string]string{
	"ACTIVE": "HOT", "INACTIVE": "COLD", "OFFLOADED": "FROZEN",
}

func tenantStatusServerView(s string) string {
	if v, ok := tenantStatusEquiv[strings.ToUpper(s)]; ok {
		return v
	}
	return strings.ToUpper(s)
}

func tenants(c *client.Client, doc manifest.Document) (Result, error) {
	td, err := doc.TenantsDoc()
	if err != nil {
		return Result{}, err
	}
	collection, err := weaviate.ResolveCollection(c, td.Collection)
	if err != nil {
		return Result{}, fmt.Errorf("collection %q: %w", td.Collection, err)
	}

	current, err := weaviate.ListTenants(c, collection)
	if err != nil {
		return Result{}, err
	}
	currentByName := map[string]weaviate.Tenant{}
	for _, t := range current {
		currentByName[t.Name] = t
	}

	res := Result{Name: collection, Action: "unchanged"}
	var toCreate, toUpdate []weaviate.Tenant
	for _, want := range td.Tenants {
		name, _ := want["name"].(string)
		status, _ := want["activityStatus"].(string)
		desired := weaviate.Tenant{Name: name, ActivityStatus: status}
		existing, ok := currentByName[name]
		if !ok {
			toCreate = append(toCreate, desired)
			continue
		}
		if tenantStatusServerView(status) != existing.ActivityStatus &&
			strings.ToUpper(status) != existing.ActivityStatus {
			toUpdate = append(toUpdate, desired)
		}
	}

	if len(toCreate) > 0 {
		if err := weaviate.CreateTenants(c, collection, toCreate); err != nil {
			return res, err
		}
		res.Action = "configured"
		res.Info = append(res.Info, fmt.Sprintf("created tenant(s): %s", tenantNames(toCreate)))
	}
	if len(toUpdate) > 0 {
		if err := weaviate.UpdateTenants(c, collection, toUpdate); err != nil {
			return res, err
		}
		res.Action = "configured"
		res.Info = append(res.Info, fmt.Sprintf("updated tenant(s): %s", tenantNames(toUpdate)))
	}
	return res, nil
}

func tenantNames(ts []weaviate.Tenant) string {
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}

// ---- backups and restore ----

// Backup and restore manifests are raw API request bodies plus a
// "backend" (and, for restore, "id") scoping field. Both operations are
// one-shot on the server: apply either starts them or reports the
// existing attempt — there is no "configured" action.

func backup(c *client.Client, doc manifest.Document) (Result, error) {
	backend := weaviate.NormalizeBackend(doc.Backend())
	body, err := weaviate.PrepareBackupBody(doc.Object)
	if err != nil {
		return Result{}, err
	}
	id := doc.BackupID()
	res := Result{Name: backend + "/" + id}

	current, err := weaviate.GetBackupStatus(c, backend, id, weaviate.StatusQuery{})
	if err == nil {
		terminal, success := weaviate.BackupPhaseDone(current.Status)
		switch {
		case success:
			res.Action = "unchanged"
			res.Info = append(res.Info,
				"backup already exists (completed at "+strOrDash(current.CompletedAt)+")")
			return res, nil
		case !terminal:
			res.Action = "unchanged"
			res.Info = append(res.Info, "backup in progress: "+current.Status)
			return res, nil
		default:
			res.Info = append(res.Info,
				"previous attempt "+current.Status+errSuffix(current.Error)+", restarting")
		}
	} else if !client.IsNotFound(err) {
		return res, err
	}

	created, err := weaviate.CreateBackup(c, backend, body)
	if err != nil {
		return res, err
	}
	res.Action = "created"
	res.Info = append(res.Info, startedInfo("backup", created.Classes))
	res.Info = append(res.Info,
		fmt.Sprintf("check progress: weavectl get backups --backend %s %s", backend, id))
	return res, nil
}

func restore(c *client.Client, doc manifest.Document) (Result, error) {
	backend := weaviate.NormalizeBackend(doc.Backend())
	id := doc.BackupID()
	if err := weaviate.ValidateBackupID(id); err != nil {
		return Result{}, err
	}
	body, err := weaviate.PrepareRestoreBody(doc.Object)
	if err != nil {
		return Result{}, err
	}
	res := Result{Name: backend + "/" + id}

	current, err := weaviate.GetRestoreStatus(c, backend, id, weaviate.StatusQuery{})
	if err == nil {
		terminal, success := weaviate.BackupPhaseDone(current.Status)
		switch {
		case success:
			res.Action = "unchanged"
			res.Info = append(res.Info, "already restored")
			return res, nil
		case !terminal:
			res.Action = "unchanged"
			res.Info = append(res.Info, "restore in progress: "+current.Status)
			return res, nil
		default:
			res.Info = append(res.Info,
				"previous attempt "+current.Status+errSuffix(current.Error)+", restarting")
		}
	} else if !client.IsNotFound(err) {
		return res, err
	}

	started, err := weaviate.RestoreBackup(c, backend, id, body)
	if err != nil {
		return res, err
	}
	res.Action = "created"
	res.Info = append(res.Info, startedInfo("restore", started.Classes))
	res.Info = append(res.Info,
		fmt.Sprintf("check progress: weavectl restore status %s %s", backend, id))
	return res, nil
}

func startedInfo(what string, classes []string) string {
	if len(classes) == 0 {
		return what + " started"
	}
	return what + " started (collections: " + strings.Join(classes, ", ") + ")"
}

// errSuffix renders a server-reported failure reason, if any.
func errSuffix(msg string) string {
	if msg == "" {
		return ""
	}
	return ": " + msg
}

func strOrDash(s string) string {
	if s == "" {
		return "unknown time"
	}
	return s
}
