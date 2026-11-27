package gui

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/cloud/auth"
)

func (r *cloudRuntime) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/cloud/admin/users", func(w http.ResponseWriter, req *http.Request) {
		out, err := r.channels.ListUsers(req.Context())
		respondCloud(w, out, err)
	})
	mux.HandleFunc("POST /api/cloud/admin/users/{id}/status", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		id, err := uuid.Parse(req.PathValue("id"))
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		var in struct{ Status string }
		if !decodeCloud(w, req, &in) {
			return
		}
		if id == p.UserID && in.Status != "active" {
			respondCloud(w, nil, errors.New("an administrator cannot disable their own account"))
			return
		}
		if err := r.channels.SetUserStatus(req.Context(), id, in.Status); err != nil {
			respondCloud(w, nil, err)
			return
		}
		respondCloud(w, map[string]bool{"ok": true}, r.channels.Audit(req.Context(), p.UserID, "user."+in.Status, "user", id.String(), map[string]string{"status": in.Status}, auth.ClientIP(req, false)))
	})
	mux.HandleFunc("POST /api/cloud/admin/wallet/credit", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		var in struct {
			UserID       uuid.UUID `json:"userId"`
			AmountMicros int64     `json:"amountMicros"`
			Reason       string    `json:"reason"`
			RequestID    string    `json:"requestId"`
		}
		if !decodeCloud(w, req, &in) {
			return
		}
		in.RequestID = strings.TrimSpace(in.RequestID)
		if len(in.RequestID) < 8 || len(in.RequestID) > 100 {
			respondCloud(w, nil, errors.New("requestId must be an 8..100 character unique idempotency key"))
			return
		}
		result, err := r.wallet.Credit(req.Context(), in.UserID, in.AmountMicros, "admin:"+in.RequestID, in.Reason)
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		if err := r.channels.Audit(req.Context(), p.UserID, "wallet.credit", "user", in.UserID.String(), map[string]any{"amountMicros": in.AmountMicros, "reason": in.Reason, "replayed": result.Replayed}, auth.ClientIP(req, false)); err != nil {
			respondCloud(w, nil, err)
			return
		}
		writeJSONStatus(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /api/cloud/admin/orders", func(w http.ResponseWriter, req *http.Request) {
		out, err := r.channels.ListOrders(req.Context(), 100)
		respondCloud(w, out, err)
	})
	mux.HandleFunc("GET /api/cloud/admin/audit", func(w http.ResponseWriter, req *http.Request) {
		out, err := r.channels.ListAudit(req.Context(), 100)
		respondCloud(w, out, err)
	})
}
