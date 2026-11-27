package gui

import (
	"net/http"

	"github.com/yetone/magpie/internal/cloud/auth"
	"github.com/yetone/magpie/internal/provider"
)

func (r *cloudRuntime) registerGroupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/cloud/groups", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		out, err := r.channels.ListUserGroups(req.Context(), p.UserID)
		respondCloud(w, out, err)
	})
	mux.HandleFunc("POST /api/cloud/groups", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		var in provider.Group
		if !decodeCloud(w, req, &in) {
			return
		}
		out, err := r.channels.CreateUserGroup(req.Context(), p.UserID, in)
		respondCloud(w, out, err)
	})
	mux.HandleFunc("DELETE /api/cloud/groups/{id}", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		respondCloud(w, map[string]bool{"ok": true}, r.channels.DeleteUserGroup(req.Context(), p.UserID, req.PathValue("id")))
	})
}
