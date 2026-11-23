// Package cfrule is an internal Anvil resource that keeps one Cloudflare
// Transform Rule adding a site's x-origin-secret header.
//
// The Pulumi Cloudflare provider only manages a zone's Transform Rules as one
// whole ruleset, which would wipe rules the user created by hand and make
// every stage overwrite the others. This resource uses Cloudflare's per-rule
// API instead: it adds its own rule, updates only that rule, and deletes only
// that rule. It is registered with the Anvil provider but not exposed in the
// SDKs; site components create it.
package cfrule

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pulumi/pulumi-go-provider/infer"
)

// Token is the resource type site components register.
const Token = "anvil:cloudflare:OriginSecretRule"

// The phase Cloudflare runs request header Transform Rules in.
const phase = "http_request_late_transform"

var apiBase = "https://api.cloudflare.com/client/v4"

// OriginSecretRule is the resource.
type OriginSecretRule struct{}

// OriginSecretRuleArgs are its inputs.
type OriginSecretRuleArgs struct {
	ZoneID    string   `pulumi:"zoneId" provider:"replaceOnChanges"`
	ZoneName  string   `pulumi:"zoneName"`
	Hostnames []string `pulumi:"hostnames" provider:"replaceOnChanges"`
	Header    string   `pulumi:"header"`
	Secret    string   `pulumi:"secret" provider:"secret"`
	// Description is shown on the rule in the Cloudflare dashboard.
	Description string `pulumi:"description"`
}

// OriginSecretRuleState is its state.
type OriginSecretRuleState struct {
	OriginSecretRuleArgs
	RulesetID string `pulumi:"rulesetId"`
	RuleID    string `pulumi:"ruleId"`
}

func (r *OriginSecretRule) Annotate(a infer.Annotator) {
	a.SetToken("cloudflare", "OriginSecretRule")
	a.Describe(r, "Internal: one Cloudflare Transform Rule adding a site's origin-protection header. Created by site components.")
}

// Create adds the rule, creating the zone's transform ruleset if it has none.
func (r *OriginSecretRule) Create(ctx context.Context, req infer.CreateRequest[OriginSecretRuleArgs]) (infer.CreateResponse[OriginSecretRuleState], error) {
	in := req.Inputs
	state := OriginSecretRuleState{OriginSecretRuleArgs: in}
	id := ruleRef(in.Hostnames)
	if req.DryRun {
		return infer.CreateResponse[OriginSecretRuleState]{ID: id, Output: state}, nil
	}
	c, err := newClient()
	if err != nil {
		return infer.CreateResponse[OriginSecretRuleState]{}, err
	}

	rule := buildRule(in)
	var ruleset rulesetResult
	status, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/zones/%s/rulesets/phases/%s/entrypoint", in.ZoneID, phase), nil, &ruleset)
	switch {
	case status == http.StatusNotFound:
		// No transform rules in this zone yet: create the entrypoint with ours.
		_, err = c.do(ctx, http.MethodPut, fmt.Sprintf("/zones/%s/rulesets/phases/%s/entrypoint", in.ZoneID, phase),
			map[string]any{"rules": []any{rule}}, &ruleset)
	case err == nil:
		_, err = c.do(ctx, http.MethodPost, fmt.Sprintf("/zones/%s/rulesets/%s/rules", in.ZoneID, ruleset.ID), rule, &ruleset)
	}
	if err != nil {
		return infer.CreateResponse[OriginSecretRuleState]{}, c.limitError(ctx, in, err)
	}

	state.RulesetID = ruleset.ID
	state.RuleID = ruleset.ruleID(id)
	if state.RuleID == "" {
		return infer.CreateResponse[OriginSecretRuleState]{}, fmt.Errorf("Cloudflare accepted the origin-protection rule for %s but didn't return it", strings.Join(in.Hostnames, ", "))
	}
	return infer.CreateResponse[OriginSecretRuleState]{ID: id, Output: state}, nil
}

// Update replaces the rule's expression and header in place.
func (r *OriginSecretRule) Update(ctx context.Context, req infer.UpdateRequest[OriginSecretRuleArgs, OriginSecretRuleState]) (infer.UpdateResponse[OriginSecretRuleState], error) {
	state := req.State
	state.OriginSecretRuleArgs = req.Inputs
	if req.DryRun {
		return infer.UpdateResponse[OriginSecretRuleState]{Output: state}, nil
	}
	c, err := newClient()
	if err != nil {
		return infer.UpdateResponse[OriginSecretRuleState]{}, err
	}
	var ruleset rulesetResult
	_, err = c.do(ctx, http.MethodPatch, fmt.Sprintf("/zones/%s/rulesets/%s/rules/%s", state.ZoneID, state.RulesetID, state.RuleID),
		buildRule(req.Inputs), &ruleset)
	if err != nil {
		return infer.UpdateResponse[OriginSecretRuleState]{}, fmt.Errorf("couldn't update the origin-protection rule in Cloudflare: %w", err)
	}
	return infer.UpdateResponse[OriginSecretRuleState]{Output: state}, nil
}

// Delete removes only this rule. A rule that's already gone is fine.
func (r *OriginSecretRule) Delete(ctx context.Context, req infer.DeleteRequest[OriginSecretRuleState]) (infer.DeleteResponse, error) {
	s := req.State
	if s.RulesetID == "" || s.RuleID == "" {
		return infer.DeleteResponse{}, nil
	}
	c, err := newClient()
	if err != nil {
		return infer.DeleteResponse{}, err
	}
	status, err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/zones/%s/rulesets/%s/rules/%s", s.ZoneID, s.RulesetID, s.RuleID), nil, nil)
	if err != nil && status != http.StatusNotFound {
		return infer.DeleteResponse{}, fmt.Errorf("couldn't delete the origin-protection rule from Cloudflare: %w", err)
	}
	return infer.DeleteResponse{}, nil
}

// ── Rule shape ──

// ruleRef identifies the rule inside the zone's ruleset. Hostnames are unique
// per zone, so the first one keeps refs unique across stages and sites.
func ruleRef(hostnames []string) string {
	h := ""
	if len(hostnames) > 0 {
		h = hostnames[0]
	}
	return "anvil-origin-" + strings.NewReplacer(".", "-", "*", "x").Replace(strings.ToLower(h))
}

// Expression matches the site's hostnames. Built with eq/or, which every plan
// supports.
func Expression(hostnames []string) string {
	parts := make([]string, len(hostnames))
	for i, h := range hostnames {
		parts[i] = fmt.Sprintf(`http.host eq %q`, strings.ToLower(h))
	}
	return "(" + strings.Join(parts, " or ") + ")"
}

func buildRule(in OriginSecretRuleArgs) map[string]any {
	return map[string]any{
		"ref":         ruleRef(in.Hostnames),
		"description": in.Description,
		"expression":  Expression(in.Hostnames),
		"action":      "rewrite",
		"enabled":     true,
		"action_parameters": map[string]any{
			"headers": map[string]any{
				in.Header: map[string]any{"operation": "set", "value": in.Secret},
			},
		},
	}
}

type rulesetResult struct {
	ID    string `json:"id"`
	Rules []struct {
		ID  string `json:"id"`
		Ref string `json:"ref"`
	} `json:"rules"`
}

func (r rulesetResult) ruleID(ref string) string {
	for _, rule := range r.Rules {
		if rule.Ref == ref {
			return rule.ID
		}
	}
	return ""
}

// ── Plan limits ──

// PlanRuleLimits are Cloudflare's active Transform Rule limits per zone, by
// plan legacy ID (developers.cloudflare.com/rules/transform/).
var PlanRuleLimits = map[string]int{"free": 10, "pro": 25, "business": 50, "enterprise": 300}

// limitError adds the zone's plan and rule limit to a failed create, since
// running out of rules is the likely cause.
func (c *client) limitError(ctx context.Context, in OriginSecretRuleArgs, apiErr error) error {
	hosts := strings.Join(in.Hostnames, ", ")
	var zone struct {
		Plan struct {
			Name     string `json:"name"`
			LegacyID string `json:"legacy_id"`
		} `json:"plan"`
	}
	planInfo := ""
	if _, err := c.do(ctx, http.MethodGet, "/zones/"+in.ZoneID, nil, &zone); err == nil && zone.Plan.Name != "" {
		planInfo = fmt.Sprintf(" %s is on the %s plan", in.ZoneName, zone.Plan.Name)
		if limit, ok := PlanRuleLimits[zone.Plan.LegacyID]; ok {
			planInfo += fmt.Sprintf(", which allows %d active Transform Rules", limit)
		}
		planInfo += "."
	}
	return fmt.Errorf("Cloudflare refused the origin-protection rule for %s: %v.%s "+
		"If the zone is out of Transform Rules, set originProtection to false for this stage, remove unused rules, "+
		"or upgrade the plan. The API token also needs Zone > Transform Rules > Edit", hosts, apiErr, planInfo)
}

// ── API client ──

type client struct {
	token string
	http  *http.Client
}

func newClient() (*client, error) {
	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("CLOUDFLARE_API_TOKEN isn't set; it's needed to manage the site's origin-protection rule in Cloudflare (on destroy too)")
	}
	return &client{token: token, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

type apiEnvelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// do calls the Cloudflare API, decoding result into out. It returns the HTTP
// status so callers can tell "not found" apart from other failures.
func (c *client) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var env apiEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return resp.StatusCode, fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if !env.Success || resp.StatusCode >= 300 {
		msgs := make([]string, len(env.Errors))
		for i, e := range env.Errors {
			msgs[i] = fmt.Sprintf("%s (code %d)", e.Message, e.Code)
		}
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.Join(msgs, "; "))
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}
