package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `spawn plugin install` surfaces a failed remote condition (#1040): spored's
// 412 becomes an error carrying the condition's message, a 200 is success, and
// an older spored without the endpoint is recognised rather than treated as a
// failure.
func TestPostPluginPreflight(t *testing.T) {
	const msg = `docker must already be present: command "command -v docker": exit status 1`
	newSpored := http.NewServeMux()
	newSpored.HandleFunc("POST /v1/plugins/preflight", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Spec string `json:"spec"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Spec, "failing") {
			w.WriteHeader(http.StatusPreconditionFailed)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "failed", "error": msg})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	// An older spored: no preflight route, but DELETE /v1/plugins/{name}
	// matches the path, so Go's mux answers 405.
	oldSpored := http.NewServeMux()
	oldSpored.HandleFunc("DELETE /v1/plugins/{name}", func(http.ResponseWriter, *http.Request) {})

	newSrv := httptest.NewServer(newSpored)
	defer newSrv.Close()
	oldSrv := httptest.NewServer(oldSpored)
	defer oldSrv.Close()

	ctx := context.Background()
	if _, err := postPluginPreflight(ctx, http.DefaultClient, newSrv.URL, "t", []byte(`{"spec":"name: passing"}`)); err != nil {
		t.Errorf("passing preflight = %v, want nil", err)
	}
	_, err := postPluginPreflight(ctx, http.DefaultClient, newSrv.URL, "t", []byte(`{"spec":"name: failing"}`))
	if !errors.Is(err, errRemoteCondition) || !strings.Contains(err.Error(), msg) {
		t.Errorf("failing preflight = %v, want errRemoteCondition carrying %q", err, msg)
	}
	if _, err := postPluginPreflight(ctx, http.DefaultClient, oldSrv.URL, "t", []byte(`{}`)); !errors.Is(err, errPreflightUnsupported) {
		t.Errorf("older spored = %v, want errPreflightUnsupported", err)
	}
}
