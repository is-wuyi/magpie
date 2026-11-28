package gui

import (
	"net/http"
	"time"

	"github.com/yetone/magpie/internal/cloud/auth"
	"github.com/yetone/magpie/internal/cloud/store"
)

type adminAuthPolicy struct {
	store.AuthPolicy
	UsersWithoutPassword int  `json:"usersWithoutPassword"`
	MailReady            bool `json:"mailReady"`
}

func (r *cloudRuntime) registerAdminAuthPolicyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/cloud/admin/auth-policy", func(w http.ResponseWriter, req *http.Request) {
		policy, err := r.store.AuthPolicy(req.Context())
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		without, err := r.store.UsersWithoutPassword(req.Context())
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		// openCloudRuntime validates and constructs the SMTP sender before it
		// starts the portal, so reaching this route means mail was configured.
		respondCloud(w, adminAuthPolicy{AuthPolicy: policy, UsersWithoutPassword: without, MailReady: true}, nil)
	})
	mux.HandleFunc("PUT /api/cloud/admin/auth-policy", func(w http.ResponseWriter, req *http.Request) {
		principal, _ := auth.PrincipalFromContext(req.Context())
		var in store.AuthPolicy
		if !decodeCloud(w, req, &in) {
			return
		}
		before, err := r.store.AuthPolicy(req.Context())
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		updated, err := r.store.SetAuthPolicy(req.Context(), in, principal.UserID, time.Now().UTC())
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		if err := r.channels.Audit(req.Context(), principal.UserID, "auth.policy.update", "auth_policy", "1", map[string]any{"before": before, "after": updated}, auth.ClientIP(req, false)); err != nil {
			respondCloud(w, nil, err)
			return
		}
		respondCloud(w, updated, nil)
	})
}
