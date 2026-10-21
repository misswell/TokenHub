package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"
)

type quotaPolicyQueryStore struct {
	Store
	listCalls int
	usageIDs  []string
}

func (s *quotaPolicyQueryStore) ListResources(kind string) []AdminResource {
	if kind == "quota-policies" {
		s.listCalls++
	}
	return s.Store.ListResources(kind)
}

func (s *quotaPolicyQueryStore) GetQuotaPolicyUsage(scope, id string) (QuotaPolicyUsage, bool, error) {
	s.usageIDs = append(s.usageIDs, id)
	return s.Store.GetQuotaPolicyUsage(scope, id)
}

func TestDOPQuotaPolicyIDQuery(t *testing.T) {
	store, app, handler := newDOPBatchTestServer(t)
	targetID := "quota/target+exact"
	for _, id := range []string{targetID, "quota-other"} {
		if _, err := store.CreateResourceChecked("quota-policies", AdminResource{
			ID: id, Name: id, Fields: map[string]any{"scope": "user", "scope_id": id},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CreateResourceChecked("budgets", AdminResource{ID: "budget-only", Name: "Budget"}); err != nil {
		t.Fatal(err)
	}
	tracked := &quotaPolicyQueryStore{Store: store}
	app.store = tracked
	for _, dop := range []bool{false, true} {
		catalog := map[string]AdminResource{}
		for _, tc := range []struct {
			name, query string
			wantIDs     []string
			wantList    int
		}{
			{"all", "", []string{targetID, "quota-other"}, 1},
			{"exact", "?id=" + url.QueryEscape(targetID), []string{targetID}, 0},
			{"missing", "?id=missing", []string{}, 0},
			{"other_kind", "?id=budget-only", []string{}, 0},
			{"literal_id", "?id=" + url.QueryEscape("' OR 1=1 --"), []string{}, 0},
			{"empty", "?id=", []string{targetID, "quota-other"}, 1},
		} {
			name := tc.name
			if dop {
				name = "dop/" + name
			}
			t.Run(name, func(t *testing.T) {
				tracked.listCalls, tracked.usageIDs = 0, nil
				r := httptest.NewRequest(http.MethodGet, "/api/admin/resources/quota-policies"+tc.query, nil)
				r.Header.Set("Authorization", "Bearer dev_admin_token")
				if dop {
					r.Header.Set("X-DOP-Operation", "true")
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("query returned %d: %s", w.Code, w.Body)
				}
				var payload struct {
					Data []AdminResource `json:"data"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				gotIDs := make([]string, 0, len(payload.Data))
				for _, item := range payload.Data {
					gotIDs = append(gotIDs, item.ID)
					if tc.query == "" {
						catalog[item.ID] = item
					} else if !reflect.DeepEqual(item, catalog[item.ID]) {
						t.Fatalf("filtered policy differs from the catalog: %s", item.ID)
					}
					if !dop && item.CurrentUsage == nil {
						t.Fatal("native query dropped current usage")
					}
					if dop && item.CurrentUsage != nil {
						t.Fatal("DOP catalog unexpectedly calculated current usage")
					}
				}
				wantList := tc.wantList
				if dop {
					wantList = 0
				}
				if !reflect.DeepEqual(gotIDs, tc.wantIDs) || tracked.listCalls != wantList {
					t.Fatalf("ids=%v, full catalog reads=%d; want %v, %d", gotIDs, tracked.listCalls, tc.wantIDs, wantList)
				}
				wantUsage := len(tc.wantIDs)
				if dop {
					wantUsage = 0
				}
				if len(tracked.usageIDs) != wantUsage {
					t.Fatalf("usage lookups=%v; want %d", tracked.usageIDs, wantUsage)
				}
			})
		}
	}
	t.Run("original_handler", func(t *testing.T) {
		response := methodRoutingRequest(app.Handler(), http.MethodGet, "/api/admin/resources/quota-policies?id="+url.QueryEscape(targetID), "dev_admin_token")
		var payload struct {
			Data []AdminResource `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || response.Code != http.StatusOK || len(payload.Data) != 2 {
			t.Fatalf("original handler changed: status=%d, count=%d, error=%v", response.Code, len(payload.Data), err)
		}
	})
	t.Run("canceled_query", func(t *testing.T) {
		for _, dop := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			r := httptest.NewRequest(http.MethodGet, "/api/admin/resources/quota-policies?id=target", nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer dev_admin_token")
			if dop {
				r.Header.Set("X-DOP-Operation", "true")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("canceled query returned %d; want 500", response.Code)
			}
		}
	})
}

func TestDOPQuotaPolicyIDQueryPreservesAuthorization(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	for _, team := range []string{"team-visible", "team-hidden"} {
		if _, err := store.CreateResourceChecked("teams", AdminResource{ID: team, Name: team}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateResourceChecked("quota-policies", AdminResource{
			ID: team, Name: team, Fields: map[string]any{"scope": "team", "scope_id": team},
		}); err != nil {
			t.Fatal(err)
		}
	}
	leader, err := store.CreateAdminUser(AdminUser{
		Username: "query-leader", Email: "query-leader@example.test", Role: "team_leader", TeamID: "team-visible", Status: StatusActive,
	}, "query-leader-password")
	if err != nil {
		t.Fatal(err)
	}
	_, session, err := store.CreateAdminSession(leader.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id    string
		count int
	}{{"team-visible", 1}, {"team-hidden", 0}} {
		response := methodRoutingRequest(handler, http.MethodGet, "/api/admin/resources/quota-policies?id="+tc.id, session.Token)
		if response.Code != http.StatusOK {
			t.Fatalf("scoped query returned %d: %s", response.Code, response.Body)
		}
		var payload struct {
			Data []AdminResource `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || len(payload.Data) != tc.count {
			t.Fatalf("scoped query returned %s, %v; want %d resources", response.Body, err, tc.count)
		}
	}
	response := methodRoutingRequest(handler, http.MethodGet, "/api/admin/resources/quota-policies?id=team-visible", "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated query returned %d; want 401", response.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/admin/resources/quota-policies?id=team-visible", nil)
	r.Header.Set("Authorization", "Bearer "+session.Token)
	r.Header.Set("X-DOP-Operation", "true")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("DOP catalog allowed a team leader: status=%d", w.Code)
	}
}
