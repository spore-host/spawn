package pluginruntime

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
)

// pluginNameRe matches valid plugin names: alphanumeric, dash, underscore; 1–64 chars.
var pluginNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

const (
	pushAPIAddr      = "127.0.0.1:7777"
	pushTokenPath    = "/var/lib/spored/push.token"
	pushTokenDirMode = 0700
	pushTokenMode    = 0600
)

// PushAPIServer is the HTTP server that receives push values from the local
// controller via an SSH-forwarded port.  It listens only on loopback.
type PushAPIServer struct {
	rt      *Runtime
	token   string
	server  *http.Server
	baseCtx context.Context // lifecycle ctx for async installs; set in Start
}

// NewPushAPIServer creates and initialises the push API server.
// It generates (or re-uses) the bearer token at /var/lib/spored/push.token.
func NewPushAPIServer(rt *Runtime) (*PushAPIServer, error) {
	token, err := ensurePushToken()
	if err != nil {
		return nil, fmt.Errorf("ensure push token: %w", err)
	}

	s := &PushAPIServer{rt: rt, token: token}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/plugins/install", s.handleInstall)
	mux.HandleFunc("POST /v1/plugins/{name}/push", s.handlePush)
	mux.HandleFunc("GET /v1/plugins/{name}/status", s.handleStatus)
	mux.HandleFunc("GET /v1/plugins", s.handleList)
	mux.HandleFunc("DELETE /v1/plugins/{name}", s.handleRemove)

	s.server = &http.Server{
		Addr:         pushAPIAddr,
		Handler:      s.authMiddleware(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}
	return s, nil
}

// Start begins serving the push API in the background.
func (s *PushAPIServer) Start(ctx context.Context) error {
	// Retain the lifecycle context so an async install kicked off by a request
	// keeps running after that request returns, and is cancelled on shutdown.
	s.baseCtx = ctx

	ln, err := net.Listen("tcp", pushAPIAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", pushAPIAddr, err)
	}

	go func() {
		log.Printf("Push API listening on %s", pushAPIAddr)
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("Push API error: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutCtx)
	}()

	return nil
}

// authMiddleware rejects requests without the correct Bearer token.
// Token comparison uses constant-time equality to prevent timing attacks.
func (s *PushAPIServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		provided := strings.TrimPrefix(auth, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// installRequest is the body of POST /v1/plugins/install. The controller sends
// the fully-resolved spec (as YAML) rather than a ref, so local-file refs work
// and there is no version skew between what the controller validated and what
// spored runs. pushed carries values captured/pushed by the controller's local
// provision steps so configure can resolve {{ pushed.<key> }} up front.
type installRequest struct {
	Spec       string             `json:"spec"` // plugin.yaml contents
	Config     map[string]string  `json:"config,omitempty"`
	Pushed     map[string]string  `json:"pushed,omitempty"`
	Provenance *plugin.Provenance `json:"provenance,omitempty"` // resolved origin/verification, recorded in on-instance state
}

// handleInstall receives POST /v1/plugins/install and runs the remote half of a
// plugin's lifecycle (install → configure → start) asynchronously. It returns
// 202 Accepted immediately; the controller polls GET /v1/plugins/{name}/status
// for the outcome. Running async keeps a slow install (e.g. a large download)
// from tripping the server's write timeout.
func (s *PushAPIServer) handleInstall(w http.ResponseWriter, r *http.Request) {
	var req installRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("install decode error: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	spec, err := plugin.ParseSpec([]byte(req.Spec))
	if err != nil {
		log.Printf("install parse spec error: %v", err)
		http.Error(w, "bad request: invalid spec", http.StatusBadRequest)
		return
	}
	if err := spec.Validate(""); err != nil {
		log.Printf("install validate spec error for %s: %v", spec.Name, err)
		http.Error(w, "bad request: spec failed validation", http.StatusBadRequest)
		return
	}

	// Run install in the background against the server's lifecycle context so it
	// survives this request but is cancelled on spored shutdown. The result is
	// observable via the status endpoint (state persisted by the Runtime).
	baseCtx := s.baseCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	go func() {
		if err := s.rt.InstallWithProvenance(baseCtx, spec, req.Config, req.Pushed, req.Provenance); err != nil {
			log.Printf("Install error for plugin %s: %v", spec.Name, err)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "installing", "name": spec.Name})
}

// handlePush receives POST /v1/plugins/{name}/push { "key": "...", "value": "..." }
func (s *PushAPIServer) handlePush(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !pluginNameRe.MatchString(name) {
		http.Error(w, "invalid plugin name", http.StatusBadRequest)
		return
	}

	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Printf("push decode error for plugin %s: %v", name, err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Key == "" {
		http.Error(w, "bad request: key required", http.StatusBadRequest)
		return
	}

	if err := s.rt.ReceivePush(r.Context(), name, body.Key, body.Value); err != nil {
		log.Printf("ReceivePush error for plugin %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleStatus serves GET /v1/plugins/{name}/status
func (s *PushAPIServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !pluginNameRe.MatchString(name) {
		http.Error(w, "invalid plugin name", http.StatusBadRequest)
		return
	}

	st, err := s.rt.Status(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// handleRemove serves DELETE /v1/plugins/{name}
func (s *PushAPIServer) handleRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !pluginNameRe.MatchString(name) {
		http.Error(w, "invalid plugin name", http.StatusBadRequest)
		return
	}
	if err := s.rt.Remove(r.Context(), name); err != nil {
		log.Printf("Remove error for plugin %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "removed"})
}

// handleList serves GET /v1/plugins
func (s *PushAPIServer) handleList(w http.ResponseWriter, r *http.Request) {
	states, err := s.rt.ListAll()
	if err != nil {
		log.Printf("ListAll error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if states == nil {
		states = []*plugin.PluginState{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(states)
}

// ensurePushToken always generates a fresh token and writes it to disk.
// A new token is produced on every spored startup so that a previously leaked
// token cannot be reused across restarts.
func ensurePushToken() (string, error) {
	// Generate a new 32-byte random token.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := hex.EncodeToString(raw)

	if err := os.MkdirAll(filepath.Dir(pushTokenPath), pushTokenDirMode); err != nil {
		return "", fmt.Errorf("create token dir: %w", err)
	}
	if err := os.WriteFile(pushTokenPath, []byte(token+"\n"), pushTokenMode); err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}

	log.Printf("Push API token written to %s", pushTokenPath)
	return token, nil
}
