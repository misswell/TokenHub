package server

import "net/http"

// Compose guarded native identity handlers only. The original Server, Store,
// gateway type assertions and original entry point remain unchanged. Never
// copy Server: it owns locks and worker lifecycles.
func (api *dopBatchAPI) registerIdentityRoutes(mux *http.ServeMux) {
	s := &Server{
		store: &dopIdentityStore{GormStore: api.store}, config: api.server.config,
		smtpRootCAs: api.server.smtpRootCAs, mux: mux,
	}
	s.registerSingleMethodRoute(http.MethodPost, "/api/admin/auth/login", s.handleAdminLogin, jsonMethodNotAllowed(http.MethodPost))
	s.registerSingleMethodRoute(http.MethodPost, "/api/admin/auth/reset-password", s.handleAdminResetPassword, jsonMethodNotAllowed(http.MethodPost))
	s.registerSingleMethodRoute(http.MethodGet, "/api/admin/auth/oauth/callback", api.oauthCallback, jsonMethodNotAllowed(http.MethodGet))
	s.registerMethodRoutes("/api/admin/users", func(allowed string) http.HandlerFunc {
		return s.adminMethodNotAllowed("identity", allowed)
	}, methodRoute{Method: http.MethodGet, Handler: s.handleAdminUsersGet}, methodRoute{Method: http.MethodPost, Handler: s.handleAdminUsersPost})
	importServer := &Server{
		store:  &dopIdentityStore{GormStore: api.store, blockManagedUpdates: true},
		config: api.server.config, smtpRootCAs: api.server.smtpRootCAs,
	}
	mux.HandleFunc(http.MethodPost+" /api/admin/users/import", importServer.handleAdminUsersImportPost)
	mux.HandleFunc("/api/admin/users/", s.handleAdminUserItem)
}
