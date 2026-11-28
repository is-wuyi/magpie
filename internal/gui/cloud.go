package gui

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/cloud/auth"
	"github.com/yetone/magpie/internal/cloud/channels"
	"github.com/yetone/magpie/internal/cloud/payment"
	"github.com/yetone/magpie/internal/cloud/store"
	"github.com/yetone/magpie/internal/cloud/wallet"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
)

type cloudRuntime struct {
	store    *store.Store
	auth     *auth.Service
	channels *channels.Service
	keys     *channels.APIKeys
	wallet   *wallet.Service
	payment  *payment.Service
	cancel   context.CancelFunc
	jobs     sync.WaitGroup
}

func openCloudRuntime(ctx context.Context) (*cloudRuntime, error) {
	cfg, err := store.ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	db, err := store.OpenMigrated(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()
	authCfg, err := auth.ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	smtpCfg, err := auth.SMTPConfigFromEnv()
	if err != nil {
		return nil, err
	}
	mailer, err := auth.NewSMTPSender(smtpCfg)
	if err != nil {
		return nil, err
	}
	accounts, err := auth.New(authCfg, db, mailer)
	if err != nil {
		return nil, err
	}
	cipher, err := channels.CipherFromEnv()
	if err != nil {
		return nil, err
	}
	r := &cloudRuntime{store: db, auth: accounts, channels: channels.New(db.Pool(), cipher), keys: channels.NewAPIKeys(db.Pool())}
	r.wallet, err = wallet.New(db.Pool())
	if err != nil {
		return nil, err
	}
	gateway.RegisterCloudBilling(r.wallet, r.channels)
	r.payment, err = paymentFromEnv(r.wallet, db.Pool())
	if err != nil {
		return nil, err
	}
	jobCtx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.jobs.Add(1)
	go r.keepCloudJobs(jobCtx)
	// These process-wide hooks are installed before the gateway starts. Both
	// registries reject a second installation rather than silently replacing
	// the authorization source while requests are running.
	provider.RegisterCloudLoader(r.channels.Load)
	provider.RegisterCloudGroupLoader(r.channels.LoadGroupCatalog)
	access.RegisterCloudAuthenticator(r.keys)
	ok = true
	return r, nil
}

func (r *cloudRuntime) Close() {
	if r.cancel != nil {
		r.cancel()
	}
	r.jobs.Wait()
	r.store.Close()
}

func (r *cloudRuntime) Handler() http.Handler {
	mux := http.NewServeMux()
	r.auth.RegisterRoutes(mux)
	r.paymentRoutes(mux)
	mux.HandleFunc("GET /api/cloud/health", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
		defer cancel()
		if err := r.store.Ping(ctx); err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		if err := provider.CloudError(); err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "provider store unavailable"})
			return
		}
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("/api/cloud/admin/", r.auth.WithPrincipal(r.auth.RequireUser(r.auth.RequireAdmin(r.adminAPI()))))
	mux.Handle("/api/cloud/", r.auth.WithPrincipal(r.auth.RequireUser(r.userAPI())))
	mux.HandleFunc("/cloud/", func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/")
		if name == "cloud/" {
			name = "cloud/index.html"
		}
		if strings.HasSuffix(name, ".html") {
			serveCloudHTML(w, name)
			return
		}
		b, err := fs.ReadFile(staticFS(), name)
		if err != nil {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		switch {
		case strings.HasSuffix(name, ".js"):
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case strings.HasSuffix(name, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		_, _ = w.Write(b)
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, req *http.Request) {
		r.serveAuthPage(w, req, "cloud/login.html")
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, req *http.Request) {
		r.serveAuthPage(w, req, "cloud/register.html")
	})
	mux.HandleFunc("/reset", func(w http.ResponseWriter, req *http.Request) {
		r.serveAuthPage(w, req, "cloud/reset.html")
	})
	mux.HandleFunc("/app", func(w http.ResponseWriter, req *http.Request) {
		if _, err := r.auth.AuthenticateRequest(req); err != nil {
			http.Redirect(w, req, "/login", http.StatusFound)
			return
		}
		serveCloudHTML(w, "cloud/portal.html")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" {
			http.NotFound(w, req)
			return
		}
		serveCloudHTML(w, "cloud/index.html")
	})
	return webOwnPage(mux)
}

const cloudPageCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func serveCloudHTML(w http.ResponseWriter, name string) {
	b, err := fs.ReadFile(staticFS(), name)
	if err != nil {
		http.Error(w, "cloud page is unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", cloudPageCSP)
	_, _ = w.Write(b)
}

func (r *cloudRuntime) serveAuthPage(w http.ResponseWriter, req *http.Request, name string) {
	if _, err := r.auth.AuthenticateRequest(req); err == nil {
		http.Redirect(w, req, "/app", http.StatusFound)
		return
	}
	serveCloudHTML(w, name)
}

func (r *cloudRuntime) userAPI() http.Handler {
	mux := http.NewServeMux()
	r.registerWalletRoutes(mux)
	r.registerGroupRoutes(mux)
	mux.HandleFunc("GET /api/cloud/providers", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		out, err := r.channels.ListUserProviders(req.Context(), p.UserID)
		respondCloud(w, out, err)
	})
	mux.HandleFunc("POST /api/cloud/providers", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		var in provider.Provider
		if !decodeCloud(w, req, &in) {
			return
		}
		out, err := r.channels.CreateUserProvider(req.Context(), p.UserID, in)
		respondCloud(w, out, err)
	})
	mux.HandleFunc("DELETE /api/cloud/providers/{id}", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		respondCloud(w, map[string]bool{"ok": true}, r.channels.DeleteUserProvider(req.Context(), p.UserID, req.PathValue("id")))
	})
	mux.HandleFunc("GET /api/cloud/house-channels", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		out, err := r.channels.ListHouseChannels(req.Context(), false)
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		type listed struct {
			channels.HouseChannel
			Granted bool `json:"granted"`
		}
		result := []listed{}
		for _, c := range out {
			granted, err := r.channels.Granted(req.Context(), p.UserID, c.ID)
			if err != nil {
				respondCloud(w, nil, err)
				return
			}
			result = append(result, listed{HouseChannel: c, Granted: granted})
		}
		writeJSONStatus(w, http.StatusOK, result)
	})
	mux.HandleFunc("POST /api/cloud/house-channels/{id}/grant", r.grantHandler(true))
	mux.HandleFunc("DELETE /api/cloud/house-channels/{id}/grant", r.grantHandler(false))
	mux.HandleFunc("GET /api/cloud/keys", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		out, err := r.keys.List(req.Context(), p.UserID)
		respondCloud(w, out, err)
	})
	mux.HandleFunc("POST /api/cloud/keys", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		var in struct {
			Name           string     `json:"name"`
			Models         []string   `json:"models"`
			HouseChannels  []string   `json:"houseChannels"`
			ExpiresAt      *time.Time `json:"expiresAt"`
			MaxConcurrency int        `json:"maxConcurrency"`
		}
		if !decodeCloud(w, req, &in) {
			return
		}
		out, err := r.keys.Create(req.Context(), p.UserID, in.Name, in.Models, in.HouseChannels, in.ExpiresAt, in.MaxConcurrency)
		respondCloud(w, out, err)
	})
	mux.HandleFunc("DELETE /api/cloud/keys/{id}", func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		id, err := uuid.Parse(req.PathValue("id"))
		if err == nil {
			err = r.keys.Revoke(req.Context(), p.UserID, id)
		}
		respondCloud(w, map[string]bool{"ok": err == nil}, err)
	})
	return mux
}

func (r *cloudRuntime) grantHandler(grant bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		p, _ := auth.PrincipalFromContext(req.Context())
		id, err := uuid.Parse(req.PathValue("id"))
		if err == nil {
			if grant {
				err = r.channels.Grant(req.Context(), p.UserID, id)
			} else {
				err = r.channels.Revoke(req.Context(), p.UserID, id)
			}
		}
		respondCloud(w, map[string]bool{"ok": err == nil}, err)
	}
}

func (r *cloudRuntime) adminAPI() http.Handler {
	mux := http.NewServeMux()
	r.registerAdminPriceRoutes(mux)
	r.registerAdminRoutes(mux)
	r.registerAdminAuthPolicyRoutes(mux)
	mux.HandleFunc("GET /api/cloud/admin/house-channels", func(w http.ResponseWriter, req *http.Request) {
		out, err := r.channels.ListHouseChannels(req.Context(), true)
		respondCloud(w, out, err)
	})
	mux.HandleFunc("POST /api/cloud/admin/house-channels", func(w http.ResponseWriter, req *http.Request) {
		var in struct{ Name, Description string }
		if !decodeCloud(w, req, &in) {
			return
		}
		out, err := r.channels.CreateHouseChannel(req.Context(), in.Name, in.Description)
		respondCloud(w, out, err)
	})
	mux.HandleFunc("POST /api/cloud/admin/house-channels/{id}/status", func(w http.ResponseWriter, req *http.Request) {
		id, err := uuid.Parse(req.PathValue("id"))
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		var in struct{ Status string }
		if !decodeCloud(w, req, &in) {
			return
		}
		respondCloud(w, map[string]bool{"ok": true}, r.channels.SetHouseStatus(req.Context(), id, in.Status))
	})
	mux.HandleFunc("GET /api/cloud/admin/house-channels/{id}/providers", func(w http.ResponseWriter, req *http.Request) {
		id, err := uuid.Parse(req.PathValue("id"))
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		out, err := r.channels.ListHouseProviders(req.Context(), id)
		respondCloud(w, out, err)
	})
	mux.HandleFunc("DELETE /api/cloud/admin/house-channels/{id}/providers/{providerID}", func(w http.ResponseWriter, req *http.Request) {
		id, err := uuid.Parse(req.PathValue("id"))
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		respondCloud(w, map[string]bool{"ok": true}, r.channels.DeleteHouseProvider(req.Context(), id, req.PathValue("providerID")))
	})
	mux.HandleFunc("POST /api/cloud/admin/house-channels/{id}/providers", func(w http.ResponseWriter, req *http.Request) {
		id, err := uuid.Parse(req.PathValue("id"))
		if err != nil {
			respondCloud(w, nil, err)
			return
		}
		var in provider.Provider
		if !decodeCloud(w, req, &in) {
			return
		}
		out, err := r.channels.CreateHouseProvider(req.Context(), id, in)
		respondCloud(w, out, err)
	})
	return mux
}

func decodeCloud(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return false
	}
	return true
}

func writeJSONStatus(w http.ResponseWriter, status int, out any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}

func respondCloud(w http.ResponseWriter, out any, err error) {
	if err == nil {
		writeJSONStatus(w, http.StatusOK, out)
		return
	}
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, channels.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, channels.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, channels.ErrInvalidAPIKey):
		status = http.StatusUnauthorized
	default:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			// Validation failures are safe to name, but database details and
			// decrypted configuration errors are not returned to the client.
			status = http.StatusBadRequest
		}
	}
	writeJSONStatus(w, status, map[string]string{"error": http.StatusText(status)})
}
