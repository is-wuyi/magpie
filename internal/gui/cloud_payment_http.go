package gui

import (
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/cloud/auth"
	"github.com/yetone/magpie/internal/cloud/payment"
	"github.com/yetone/magpie/internal/cloud/pricing"
)

func (r *cloudRuntime) paymentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/payments/alipay/notify", func(w http.ResponseWriter, req *http.Request) {
		if r.payment == nil {
			http.Error(w, "failure", http.StatusServiceUnavailable)
			return
		}
		req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
		if err := req.ParseForm(); err != nil {
			http.Error(w, "failure", http.StatusBadRequest)
			return
		}
		result, err := r.payment.HandleAlipayNotify(req.Context(), req.Form)
		if err != nil || !result.Ack {
			http.Error(w, "failure", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "success")
	})
	mux.HandleFunc("POST /api/payments/wechat/notify", func(w http.ResponseWriter, req *http.Request) {
		if r.payment == nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"code": "UNAVAILABLE"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, payment.MaxNotificationBytes))
		if err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "INVALID_REQUEST"})
			return
		}
		result, err := r.payment.HandleWeChatNotify(req.Context(), req.Header, body)
		if err != nil || !result.Ack {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "FAIL"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (r *cloudRuntime) registerWalletRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/cloud/usage", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		out, err := r.wallet.ListUsage(req.Context(), p.UserID, 100, 0)
		respondCloud(w, out, err)
	})
	mux.HandleFunc("GET /api/cloud/wallet", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		balance, err := r.wallet.Balance(req.Context(), p.UserID)
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		out := map[string]any{"balanceMicros": balance.BalanceMicros, "heldMicros": balance.HeldMicros, "availableMicros": balance.AvailableMicros, "currency": pricing.CurrencyCNY, "paymentsEnabled": r.payment != nil}
		if r.payment != nil {
			out["presets"] = r.payment.Presets()
			out["providers"] = r.payment.AvailableProviders()
		}
		writeJSONStatus(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/cloud/orders", func(w http.ResponseWriter, req *http.Request) {
		if r.payment == nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "payments are not configured"})
			return
		}
		p, _ := auth.PrincipalFromContext(req.Context())
		out, err := r.payment.OrdersForUser(req.Context(), p.UserID, 100)
		respondCloud(w, out, err)
	})
	mux.HandleFunc("POST /api/cloud/orders", func(w http.ResponseWriter, req *http.Request) {
		if r.payment == nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "payments are not configured"})
			return
		}
		p, _ := auth.PrincipalFromContext(req.Context())
		var in struct {
			Provider payment.Provider `json:"provider"`
			PresetID string           `json:"presetId"`
		}
		if !decodeCloud(w, req, &in) {
			return
		}
		checkout, err := r.payment.CreateOrder(req.Context(), p.UserID, in.Provider, in.PresetID)
		if err != nil && checkout.Order.ID == uuid.Nil {
			respondCloud(w, nil, err)
			return
		}
		// A provider API failure after the order insert is returned separately;
		// the pending order can be retried or reconciled by its merchant number.
		writeJSONStatus(w, http.StatusOK, map[string]any{"checkout": checkout, "providerError": errorText(err)})
	})
	mux.HandleFunc("GET /api/cloud/orders/{merchantNo}", func(w http.ResponseWriter, req *http.Request) {
		if r.payment == nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "payments are not configured"})
			return
		}
		p, _ := auth.PrincipalFromContext(req.Context())
		order, err := r.payment.Order(req.Context(), req.PathValue("merchantNo"))
		if err == nil && order.UserID != p.UserID {
			err = payment.ErrOrderNotFound
		}
		respondCloud(w, order, err)
	})
	mux.HandleFunc("GET /api/cloud/house-channels/{id}/prices", func(w http.ResponseWriter, req *http.Request) {
		id, err := uuid.Parse(req.PathValue("id"))
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		out, err := r.channels.ListHousePrices(req.Context(), id)
		respondCloud(w, out, err)
	})
}

func (r *cloudRuntime) registerAdminPriceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/cloud/admin/house-channels/{id}/prices", func(w http.ResponseWriter, req *http.Request) {
		id, err := uuid.Parse(req.PathValue("id"))
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		out, err := r.channels.ListHousePrices(req.Context(), id)
		respondCloud(w, out, err)
	})
	mux.HandleFunc("PUT /api/cloud/admin/house-channels/{id}/prices", func(w http.ResponseWriter, req *http.Request) {
		id, err := uuid.Parse(req.PathValue("id"))
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		var in struct {
			Provider string        `json:"provider"`
			Model    string        `json:"model"`
			Price    pricing.Price `json:"price"`
		}
		if !decodeCloud(w, req, &in) {
			return
		}
		respondCloud(w, map[string]bool{"ok": true}, r.channels.SetHousePrice(req.Context(), id, in.Provider, in.Model, in.Price))
	})
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return errors.New("payment provider could not create the checkout; use the pending order for reconciliation").Error()
}
