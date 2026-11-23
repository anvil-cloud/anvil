package cfrule

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pulumi/pulumi-go-provider/infer"
)

type call struct{ method, path, body string }

// fakeAPI records calls and answers from a route table.
func fakeAPI(t *testing.T, routes map[string]func(body string) (int, any)) (*[]call, func()) {
	t.Helper()
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, call{r.Method, r.URL.Path, string(b)})
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing bearer token on %s %s", r.Method, r.URL.Path)
		}
		h, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 1, "message": "unexpected " + r.Method + " " + r.URL.Path}}})
			return
		}
		status, result := h(string(b))
		w.WriteHeader(status)
		if status >= 300 {
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": result})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
	}))
	old := apiBase
	apiBase = srv.URL
	t.Setenv("CLOUDFLARE_API_TOKEN", "test-token")
	return &calls, func() { apiBase = old; srv.Close() }
}

var args = OriginSecretRuleArgs{
	ZoneID: "z1", ZoneName: "youlosei.win",
	Hostnames: []string{"youlosei.win", "www.youlosei.win"},
	Header:    "x-origin-secret", Secret: "s3cret", Description: "Managed by Anvil",
}

const entry = "/zones/z1/rulesets/phases/http_request_late_transform/entrypoint"

func TestCreateAddsRuleToExistingRuleset(t *testing.T) {
	calls, done := fakeAPI(t, map[string]func(string) (int, any){
		"GET " + entry: func(string) (int, any) {
			return 200, map[string]any{"id": "rs1", "rules": []any{map[string]any{"id": "user-rule", "ref": "mine"}}}
		},
		"POST /zones/z1/rulesets/rs1/rules": func(body string) (int, any) {
			return 200, map[string]any{"id": "rs1", "rules": []any{
				map[string]any{"id": "user-rule", "ref": "mine"},
				map[string]any{"id": "r9", "ref": "anvil-origin-youlosei-win"},
			}}
		},
	})
	defer done()

	resp, err := (&OriginSecretRule{}).Create(context.Background(), infer.CreateRequest[OriginSecretRuleArgs]{Inputs: args})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Output.RulesetID != "rs1" || resp.Output.RuleID != "r9" {
		t.Fatalf("state = %+v", resp.Output)
	}
	post := (*calls)[1].body
	for _, want := range []string{`http.host eq \"youlosei.win\" or http.host eq \"www.youlosei.win\"`, `"x-origin-secret":{"operation":"set","value":"s3cret"}`, `"action":"rewrite"`} {
		if !strings.Contains(post, want) {
			t.Errorf("rule body missing %s: %s", want, post)
		}
	}
}

func TestCreateMakesEntrypointWhenZoneHasNone(t *testing.T) {
	calls, done := fakeAPI(t, map[string]func(string) (int, any){
		"GET " + entry: func(string) (int, any) { return 404, []any{map[string]any{"code": 10003, "message": "not found"}} },
		"PUT " + entry: func(string) (int, any) {
			return 200, map[string]any{"id": "rs2", "rules": []any{map[string]any{"id": "r1", "ref": "anvil-origin-youlosei-win"}}}
		},
	})
	defer done()

	resp, err := (&OriginSecretRule{}).Create(context.Background(), infer.CreateRequest[OriginSecretRuleArgs]{Inputs: args})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Output.RuleID != "r1" || (*calls)[1].method != "PUT" {
		t.Fatalf("state %+v, calls %+v", resp.Output, *calls)
	}
}

func TestCreateLimitErrorNamesPlan(t *testing.T) {
	_, done := fakeAPI(t, map[string]func(string) (int, any){
		"GET " + entry: func(string) (int, any) { return 200, map[string]any{"id": "rs1", "rules": []any{}} },
		"POST /zones/z1/rulesets/rs1/rules": func(string) (int, any) {
			return 400, []any{map[string]any{"code": 20217, "message": "exceeded maximum number of rules"}}
		},
		"GET /zones/z1": func(string) (int, any) {
			return 200, map[string]any{"plan": map[string]any{"name": "Free Website", "legacy_id": "free"}}
		},
	})
	defer done()

	_, err := (&OriginSecretRule{}).Create(context.Background(), infer.CreateRequest[OriginSecretRuleArgs]{Inputs: args})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"Free Website", "allows 10 active Transform Rules", "originProtection to false", "Transform Rules > Edit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestCreateDryRunCallsNothing(t *testing.T) {
	calls, done := fakeAPI(t, nil)
	defer done()
	if _, err := (&OriginSecretRule{}).Create(context.Background(), infer.CreateRequest[OriginSecretRuleArgs]{Inputs: args, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("preview called the API: %+v", *calls)
	}
}

func TestDeleteOnlyItsRuleAndToleratesMissing(t *testing.T) {
	calls, done := fakeAPI(t, map[string]func(string) (int, any){
		"DELETE /zones/z1/rulesets/rs1/rules/r9": func(string) (int, any) {
			return 404, []any{map[string]any{"code": 10003, "message": "not found"}}
		},
	})
	defer done()
	state := OriginSecretRuleState{OriginSecretRuleArgs: args, RulesetID: "rs1", RuleID: "r9"}
	if _, err := (&OriginSecretRule{}).Delete(context.Background(), infer.DeleteRequest[OriginSecretRuleState]{State: state}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0].method != "DELETE" {
		t.Fatalf("calls %+v", *calls)
	}
}

func TestMissingTokenFails(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	_, err := (&OriginSecretRule{}).Create(context.Background(), infer.CreateRequest[OriginSecretRuleArgs]{Inputs: args})
	if err == nil || !strings.Contains(err.Error(), "CLOUDFLARE_API_TOKEN") {
		t.Fatalf("err = %v", err)
	}
}
