// Package pushseal encrypts a plugin push-API call end to end, for the one
// transport that would otherwise record it in clear: SSM SendCommand.
//
// spawn reaches spored's loopback push API over SSH when it can, and otherwise
// by running curl on the instance over SSM (#855). SSM keeps every command's
// parameters and output in its command history, readable by anyone in the
// account with ssm:ListCommands / ssm:GetCommandInvocation, for weeks. A plugin
// install carries its config (passwords) and pushed values (registration
// tokens, auth keys), and the status and list answers echo them back.
//
// So over SSM a call travels sealed, with HPKE (RFC 9180) in both directions:
//
//  1. spawn asks spored for a fresh key (GET KeyPath). spored generates a
//     single-use keypair, keeps the private half in memory, and answers with
//     the public half, which SSM's history may hold.
//  2. spawn seals {method, path, body, reply key} to it, where the reply key is
//     a keypair spawn generated for this one call, and POSTs the ciphertext to
//     SealedPath.
//  3. spored opens it, deleting the private key as it does so (one use, and an
//     unused one expires after KeyTTL), serves the inner request, and seals the
//     answer to the reply key.
//
// Both private keys exist only in memory for the length of one call. What SSM
// records, the public keys and the two ciphertexts, can never be opened, by
// anyone, afterwards.
//
// The ciphersuite is the ML-KEM-768 + X25519 hybrid KEM, because the threat
// is history that is kept: a recording made today must stay sealed later.
package pushseal

import (
	"bytes"
	"crypto/hpke"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// KeyPath is where spored hands out a single-use public key.
	KeyPath = "/v1/seal-key"
	// SealedPath is where a sealed call is posted.
	SealedPath = "/v1/sealed"

	// KeyTTL is how long an unused key is kept.
	KeyTTL = 5 * time.Minute
	// maxKeys caps the outstanding keys; the oldest goes first.
	maxKeys = 16
	// maxBody caps a sealed request or inner response.
	maxBody = 4 << 20
)

var (
	requestInfo = []byte("spore.host spawn push-api sealed request v1")
	replyInfo   = []byte("spore.host spawn push-api sealed reply v1")
)

func kem() hpke.KEM   { return hpke.MLKEM768X25519() }
func kdf() hpke.KDF   { return hpke.HKDFSHA256() }
func aead() hpke.AEAD { return hpke.AES256GCM() }
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Offer is spored's answer at KeyPath.
type Offer struct {
	ID        string `json:"id"`
	PublicKey []byte `json:"public_key"`
}

// envelope is the body posted to SealedPath.
type envelope struct {
	KeyID  string `json:"key_id"`
	Sealed []byte `json:"sealed"`
}

// inner is the sealed request.
type inner struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	Body     []byte `json:"body,omitempty"`
	ReplyKey []byte `json:"reply_key"`
}

// replyEnvelope is SealedPath's answer.
type replyEnvelope struct {
	Sealed []byte `json:"sealed"`
}

// innerReply is the sealed answer.
type innerReply struct {
	Status int    `json:"status"`
	Body   []byte `json:"body,omitempty"`
}

// ── client ──────────────────────────────────────────────────────────────────

// Seal builds the body to post to SealedPath for one call, and the function
// that opens SealedPath's answer into the inner status and body.
func Seal(offerJSON []byte, method, path string, body []byte) ([]byte, func(replyJSON []byte) (int, []byte, error), error) {
	var offer Offer
	if err := json.Unmarshal(offerJSON, &offer); err != nil || offer.ID == "" || len(offer.PublicKey) == 0 {
		return nil, nil, fmt.Errorf("unreadable seal key from spored: %v", err)
	}
	pub, err := kem().NewPublicKey(offer.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("seal key from spored: %w", err)
	}
	reply, err := kem().GenerateKey()
	if err != nil {
		return nil, nil, err
	}
	pt, err := json.Marshal(inner{Method: method, Path: path, Body: body, ReplyKey: reply.PublicKey().Bytes()})
	if err != nil {
		return nil, nil, err
	}
	ct, err := hpke.Seal(pub, kdf(), aead(), requestInfo, pt)
	if err != nil {
		return nil, nil, err
	}
	env, err := json.Marshal(envelope{KeyID: offer.ID, Sealed: ct})
	if err != nil {
		return nil, nil, err
	}
	open := func(replyJSON []byte) (int, []byte, error) {
		var re replyEnvelope
		if err := json.Unmarshal(replyJSON, &re); err != nil || len(re.Sealed) == 0 {
			return 0, nil, fmt.Errorf("unreadable sealed answer from spored: %v", err)
		}
		pt, err := hpke.Open(reply, kdf(), aead(), replyInfo, re.Sealed)
		if err != nil {
			return 0, nil, fmt.Errorf("open sealed answer: %w", err)
		}
		var ir innerReply
		if err := json.Unmarshal(pt, &ir); err != nil {
			return 0, nil, fmt.Errorf("sealed answer: %w", err)
		}
		return ir.Status, ir.Body, nil
	}
	return env, open, nil
}

// ── server ──────────────────────────────────────────────────────────────────

type heldKey struct {
	key     hpke.PrivateKey
	created time.Time
}

// Server serves KeyPath and SealedPath in front of the push API's own routes.
// It does no authentication: wrap it, as the push API's bearer-token middleware
// does.
type Server struct {
	inner http.Handler
	// Now is the clock; nil means time.Now. Tests set it.
	Now func() time.Time

	mu   sync.Mutex
	keys map[string]heldKey
}

// NewServer serves sealed calls to inner, and every other request by inner
// itself.
func NewServer(inner http.Handler) *Server {
	return &Server{inner: inner, keys: map[string]heldKey{}}
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == KeyPath && r.Method == http.MethodGet:
		s.serveKey(w)
	case r.URL.Path == SealedPath && r.Method == http.MethodPost:
		s.serveSealed(w, r)
	case r.URL.Path == KeyPath || r.URL.Path == SealedPath:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		s.inner.ServeHTTP(w, r)
	}
}

func (s *Server) serveKey(w http.ResponseWriter) {
	k, err := kem().GenerateKey()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	id, err := newID()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.pruneLocked()
	for len(s.keys) >= maxKeys {
		oldest := ""
		for kid, hk := range s.keys {
			if oldest == "" || hk.created.Before(s.keys[oldest].created) {
				oldest = kid
			}
		}
		delete(s.keys, oldest)
	}
	s.keys[id] = heldKey{key: k, created: s.now()}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Offer{ID: id, PublicKey: k.PublicKey().Bytes()})
}

func (s *Server) pruneLocked() {
	for id, hk := range s.keys {
		if s.now().Sub(hk.created) > KeyTTL {
			delete(s.keys, id)
		}
	}
}

// take removes and returns a held key: a key opens one call at most.
func (s *Server) take(id string) (hpke.PrivateKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	hk, ok := s.keys[id]
	delete(s.keys, id)
	return hk.key, ok
}

// outstanding is how many keys are held. For tests.
func (s *Server) outstanding() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}

func (s *Server) serveSealed(w http.ResponseWriter, r *http.Request) {
	var env envelope
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&env); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key, ok := s.take(env.KeyID)
	if !ok {
		http.Error(w, "unknown or expired seal key", http.StatusBadRequest)
		return
	}
	pt, err := hpke.Open(key, kdf(), aead(), requestInfo, env.Sealed)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var in inner
	if err := json.Unmarshal(pt, &in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	replyKey, err := kem().NewPublicKey(in.ReplyKey)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := checkInner(in.Method, in.Path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), in.Method, in.Path, bytes.NewReader(in.Body))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if in.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := newRecorder()
	s.inner.ServeHTTP(rec, req)

	out, err := json.Marshal(innerReply{Status: rec.status(), Body: rec.body.Bytes()})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ct, err := hpke.Seal(replyKey, kdf(), aead(), replyInfo, out)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(replyEnvelope{Sealed: ct})
}

// checkInner admits only a push-API route: no nesting, no absolute URL.
func checkInner(method, path string) error {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodDelete:
	default:
		return errors.New("sealed call: method not allowed")
	}
	if !strings.HasPrefix(path, "/v1/") || path == KeyPath || path == SealedPath {
		return errors.New("sealed call: path not allowed")
	}
	return nil
}

// recorder is the minimal http.ResponseWriter the inner handler writes to.
type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func newRecorder() *recorder { return &recorder{header: http.Header{}} }

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	if r.body.Len()+len(b) > maxBody {
		return 0, errors.New("sealed call: response too large")
	}
	return r.body.Write(b)
}

func (r *recorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}
