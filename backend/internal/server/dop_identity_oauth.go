package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Keep the native state cookie, one-time flow, provider PKCE and browser PKCE
// exchange. Only identity resolution differs for pre-provisioned employees.
func (api *dopBatchAPI) oauthCallback(w http.ResponseWriter, r *http.Request) {
	s := api.server
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	cookie, err := r.Cookie(adminOAuthStateCookieName(state))
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		writeError(w, r, NewHTTPError(400, "invalid_oauth_state", "OAuth state is invalid or expired"))
		return
	}
	flow, ok, err := api.store.ConsumeAdminOAuthFlow(state, cookie.Value)
	if err != nil {
		writeError(w, r, dopBatchSafeError(err))
		return
	}
	if !ok {
		writeError(w, r, NewHTTPError(400, "invalid_oauth_state", "OAuth state is invalid or expired"))
		return
	}
	clearAdminOAuthBindingCookie(w, adminOAuthStateCookieName(state), adminOAuthStateCookiePath, flow.CookieSecure)
	fail := func(code string) { http.Redirect(w, r, oauthRedirectWithError(flow.ReturnURL, code), http.StatusFound) }
	if r.URL.Query().Get("error") != "" {
		fail("provider_error")
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		code = strings.TrimSpace(r.URL.Query().Get("authCode"))
	}
	if code == "" {
		fail("missing_code")
		return
	}
	provider, ok := s.findActiveOAuthIdentityProvider(flow.ProviderID)
	if !ok {
		fail("identity_provider_not_found")
		return
	}
	token, err := s.exchangeOAuthCode(r.Context(), provider, code, flow.RedirectURI, flow.ProviderCodeVerifier)
	if err != nil {
		fail("token_exchange_failed")
		return
	}
	claims, err := s.fetchOAuthUserInfo(r.Context(), provider, token.AccessToken, code)
	if err != nil {
		fail("userinfo_failed")
		return
	}
	user, err := api.resolveOAuthUser(provider, claims)
	if err != nil {
		fail("user_sync_failed")
		return
	}
	exchangeCode, err := randomHex(32)
	if err != nil {
		fail("session_failed")
		return
	}
	if err := api.store.SaveAdminOAuthExchange(adminOAuthExchange{
		Code: exchangeCode, CodeChallenge: flow.CodeChallenge, UserID: user.ID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		fail("session_failed")
		return
	}
	http.Redirect(w, r, oauthRedirectWithCode(flow.ReturnURL, exchangeCode), http.StatusFound)
}

func (api *dopBatchAPI) resolveOAuthUser(provider AdminResource, claims map[string]any) (AdminUser, error) {
	// This contract uses OIDC's stable, case-sensitive sub from the configured
	// provider's userinfo endpoint. Do not fall back to email, display name or a
	// username claim. Other native provider flows remain available for other users.
	subject, _ := claims["sub"].(string)
	var binding dopOAuthIdentity
	err := api.store.db.Where("provider_id = ? AND subject = ?", provider.ID, subject).Take(&binding).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return AdminUser{}, err
	}
	if err == nil && subject != "" {
		if verified, present := oauthEmailVerification(claims); present && !verified {
			return AdminUser{}, NewHTTPError(403, "oauth_email_unverified", "OAuth provider did not verify the account email")
		}
		var user AdminUser
		if err := api.store.db.First(&user, "id = ?", binding.UserID).Error; err != nil {
			return AdminUser{}, dopOAuthOnlyError()
		}
		if binding.ProviderID == dopNoLoginProvider || user.Status != StatusActive || user.Role != "user" || user.PasswordHash != dopOAuthOnlyPassword {
			return AdminUser{}, dopOAuthOnlyError()
		}
		// DOP owns profile, organization and role. OAuth claims never overwrite
		// them, and a changed provider email never changes identity ownership.
		return publicAdminUser(user), nil
	}
	// Preserve native behavior for non-managed accounts, but refuse all native
	// email-based updates of a managed identity (wrong provider/sub included).
	s := &Server{store: &dopIdentityStore{GormStore: api.store, blockManagedUpdates: true}, config: api.server.config}
	return s.upsertOAuthAdminUser(provider, claims)
}
