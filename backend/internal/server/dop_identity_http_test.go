package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDOPIdentityOAuthHTTPAndOneTimePKCE(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	item := dopProvisionFixture(t, store, "employee")
	result := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "oauth-http", dopTestItems(item)))[0]
	var value dopProvisionTestResult
	if result.Status != 201 || json.Unmarshal(result.Body, &value) != nil {
		t.Fatalf("provision failed: %s", result.Body)
	}
	providerAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil || r.PostForm.Get("code_verifier") == "" {
				t.Error("provider PKCE verifier missing")
			}
			writeJSON(w, 200, map[string]any{"access_token": "synthetic-access"})
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer synthetic-access" {
				t.Error("provider access token missing")
			}
			writeJSON(w, 200, map[string]any{"sub": "employee", "email": "employee@example.test", "email_verified": true})
		default:
			t.Error("unexpected provider path")
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(providerAPI.Close)
	_, err := store.UpdateResource("identity-providers", "dop-oidc", AdminResource{Fields: map[string]any{
		"provider_type": "oidc", "client_id": "test-client", "authorize_url": "https://id.example.test/authorize",
		"token_url": providerAPI.URL + "/token", "userinfo_url": providerAPI.URL + "/userinfo",
		"redirect_uri": "http://localhost/api/admin/auth/oauth/callback",
	}})
	if err != nil {
		t.Fatal(err)
	}
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, httptest.NewRequest(http.MethodGet, adminOAuthStartURLForTest(t, "http://localhost/api/admin/auth/oauth/start?id=dop-oidc"), nil))
	if start.Code != http.StatusFound {
		t.Fatalf("OAuth start failed: %d %s", start.Code, start.Body)
	}
	target, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	cookie := requireResponseCookieWithPrefix(t, start, adminOAuthStateCookiePrefix)
	callbackURL := "http://localhost/api/admin/auth/oauth/callback?code=synthetic-code&state=" + url.QueryEscape(target.Query().Get("state"))
	missingCookie := httptest.NewRecorder()
	handler.ServeHTTP(missingCookie, httptest.NewRequest(http.MethodGet, callbackURL, nil))
	if missingCookie.Code != 400 {
		t.Fatal("callback accepted missing browser binding")
	}
	callback := httptest.NewRequest(http.MethodGet, callbackURL, nil)
	callback.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, callback)
	redirect, _ := url.Parse(response.Header().Get("Location"))
	fragment, _ := url.ParseQuery(redirect.Fragment)
	code := fragment.Get("oauth_code")
	if code == "" || strings.Contains(redirect.String(), "error") {
		t.Fatalf("OAuth callback failed: status=%d error=%s", response.Code, fragment.Get("oauth_error"))
	}
	session := exchangeAdminOAuthCodeForTest(t, handler, code, testAdminOAuthCodeVerifier)
	if session.User.ID != value.User.ID || len(store.ListAdminUsers()) != 2 {
		t.Fatal("OAuth created a duplicate or authenticated the wrong employee")
	}
	replay := doJSON(t, handler, http.MethodPost, "/api/admin/auth/oauth/exchange", map[string]any{"code": code, "code_verifier": testAdminOAuthCodeVerifier}, "")
	if replay.Code != 400 {
		t.Fatal("OAuth exchange was not single-use")
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, callback)
	if response.Code != 400 {
		t.Fatal("OAuth state was not single-use")
	}
}
