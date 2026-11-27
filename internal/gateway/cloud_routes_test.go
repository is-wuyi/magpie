package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yetone/magpie/internal/cloud"
)

func TestCloudRoutesLeaveNoLegacyManagementPlane(t *testing.T) {
	cloud.SetEnabled(true)
	t.Cleanup(func() { cloud.SetEnabled(false) })
	h := cloudRouteGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, path := range []string{"/v1/magpie/admin/providers", "/v1/magpie/keys/oidc", "/v1/magpie/join", "/mcp/magpie", "/_magpie/plugin/x", JoinPagePath, ChatPagePath} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s reached the legacy plane: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/magpie/limit", "/v1/magpie/usage/requests", KeyPagePath} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Errorf("cloud inference/self-service route %s was blocked: %d", path, w.Code)
		}
	}
}
