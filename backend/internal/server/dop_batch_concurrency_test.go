package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestDOPBatchOptionalConcurrencyPreservesSerialDefault(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	team := store.CreateResource("teams", AdminResource{Name: "Company"})
	item := dopTestItem("team", "POST", "", AdminResource{Name: "Budget", Fields: map[string]any{"scope": "team", "scope_id": team.ID, "amount_usd": 100, "enforcement": "hard"}})
	for i, value := range []any{nil, 1, 4} {
		body := dopTestItems(item)
		if value != nil {
			body["concurrency"] = value
		}
		response := dopBatchRequest(t, handler, "team-budgets", fmt.Sprint(i), body)
		var envelope map[string]json.RawMessage
		if json.Unmarshal(response.Body.Bytes(), &envelope) != nil || response.Code != 200 {
			t.Fatalf("compatible request failed: %s", response.Body)
		}
		if value == nil {
			if _, ok := envelope["execution"]; ok {
				t.Fatal("legacy response changed")
			}
		} else {
			var execution struct {
				Concurrency int
				Mode        string
			}
			if json.Unmarshal(envelope["execution"], &execution) != nil || execution.Concurrency != 1 || execution.Mode != "serial" {
				t.Fatal("SQLite did not retain single-writer execution")
			}
		}
	}
	before := len(store.ListResources("budgets"))
	for _, value := range []any{0, -1, 9, 1.5, "4"} {
		body := dopTestItems(item)
		body["concurrency"] = value
		if response := dopBatchRequest(t, handler, "team-budgets", "invalid", body); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid concurrency accepted: %v", value)
		}
	}
	if len(store.ListResources("budgets")) != before {
		t.Fatal("invalid concurrency wrote data")
	}
}

func TestDOPBatchConcurrencyOptions(t *testing.T) {
	for _, name := range []string{"TOKENHUB_DOP_BATCH_MAX_CONCURRENCY", "TOKENHUB_DOP_BATCH_MAX_INFLIGHT", "TOKENHUB_DOP_BATCH_MAX_REQUESTS", "TOKENHUB_DOP_BATCH_RESERVED_CONNECTIONS", "TOKENHUB_DOP_BATCH_TIMEOUT_SECONDS", "TOKENHUB_DOP_BATCH_PARALLEL_PROVISION"} {
		t.Setenv(name, "")
	}
	// Empty explicitly configured capacities are rejected instead of silently
	// changing service limits. With no environment, options use safe defaults.
	if _, err := DOPBatchOptionsFromEnv(); err == nil {
		t.Fatal("empty explicit limits accepted")
	}
	for _, test := range []DOPBatchOptions{{MaxConcurrency: -1}, {MaxConcurrency: 33}, {MaxInFlight: 65}, {MaxRequests: 9}} {
		if _, err := test.normalized(); err == nil {
			t.Fatal("invalid capacity accepted")
		}
	}
	options, err := (DOPBatchOptions{}).normalized()
	if err != nil || options.MaxConcurrency != 8 || options.MaxRequests != 2 || options.MaxInFlight != 8 {
		t.Fatal("unexpected defaults")
	}
	t.Setenv("TOKENHUB_DOP_BATCH_MAX_CONCURRENCY", "8")
	t.Setenv("TOKENHUB_DOP_BATCH_MAX_INFLIGHT", "8")
	t.Setenv("TOKENHUB_DOP_BATCH_MAX_REQUESTS", "2")
	t.Setenv("TOKENHUB_DOP_BATCH_RESERVED_CONNECTIONS", "0")
	t.Setenv("TOKENHUB_DOP_BATCH_TIMEOUT_SECONDS", "300")
	t.Setenv("TOKENHUB_DOP_BATCH_PARALLEL_PROVISION", "false")
	options, err = DOPBatchOptionsFromEnv()
	if err != nil || !options.DisableParallelProvision || *options.ReservedConnections != 0 {
		t.Fatal("environment options were not applied")
	}
}
