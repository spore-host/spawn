package pushseal

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// echo is an inner push API that answers with what it got.
var echo = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	w.WriteHeader(http.StatusTeapot)
	_, _ = w.Write([]byte(r.Method + " " + r.URL.Path + " " + string(b) + " ct=" + r.Header.Get("Content-Type"))) // nosemgrep
})

func do(t *testing.T, h http.Handler, method, path string, body []byte) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewReader(body)))
	return rec.Code, rec.Body.Bytes()
}

func sealedCall(t *testing.T, s *Server, method, path string, body []byte) (int, []byte, []byte, []byte) {
	t.Helper()
	code, offer := do(t, s, http.MethodGet, KeyPath, nil)
	if code != http.StatusOK {
		t.Fatalf("key: %d %s", code, offer)
	}
	env, open, err := Seal(offer, method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	code, reply := do(t, s, http.MethodPost, SealedPath, env)
	if code != http.StatusOK {
		t.Fatalf("sealed: %d %s", code, reply)
	}
	st, got, err := open(reply)
	if err != nil {
		t.Fatal(err)
	}
	return st, got, env, reply
}

func TestSealedRoundTrip(t *testing.T) {
	s := NewServer(echo)
	secret := []byte(`{"config":{"password":"hunter2"}}`)
	st, got, env, reply := sealedCall(t, s, http.MethodPost, "/v1/plugins/install", secret)
	if st != http.StatusTeapot || string(got) != "POST /v1/plugins/install "+string(secret)+" ct=application/json" {
		t.Errorf("inner answer = %d %q", st, got)
	}
	// What travels (and what SSM would record) carries neither the request nor
	// the answer in any readable form.
	for _, wire := range [][]byte{env, reply} {
		if bytes.Contains(wire, []byte("hunter2")) || bytes.Contains(wire, []byte("/v1/plugins/install")) {
			t.Errorf("plaintext on the wire: %s", wire)
		}
	}
	if s.outstanding() != 0 {
		t.Errorf("%d keys held after the call; a key opens one call", s.outstanding())
	}
}

func TestSealedKeyIsSingleUseAndExpires(t *testing.T) {
	now := time.Now()
	s := NewServer(echo)
	s.Now = func() time.Time { return now }

	_, offer := do(t, s, http.MethodGet, KeyPath, nil)
	env, _, _ := Seal(offer, http.MethodGet, "/v1/plugins", nil)
	if code, _ := do(t, s, http.MethodPost, SealedPath, env); code != http.StatusOK {
		t.Fatalf("first use: %d", code)
	}
	if code, _ := do(t, s, http.MethodPost, SealedPath, env); code != http.StatusBadRequest {
		t.Errorf("replay: %d, want 400", code)
	}

	_, offer = do(t, s, http.MethodGet, KeyPath, nil)
	env, _, _ = Seal(offer, http.MethodGet, "/v1/plugins", nil)
	now = now.Add(KeyTTL + time.Second)
	if code, _ := do(t, s, http.MethodPost, SealedPath, env); code != http.StatusBadRequest {
		t.Errorf("expired key: %d, want 400", code)
	}
}

func TestSealedKeysAreCapped(t *testing.T) {
	s := NewServer(echo)
	for i := 0; i < 3*maxKeys; i++ {
		do(t, s, http.MethodGet, KeyPath, nil)
	}
	if n := s.outstanding(); n > maxKeys {
		t.Errorf("%d keys held, cap %d", n, maxKeys)
	}
}

func TestSealedRefusesTamperingAndOtherRoutes(t *testing.T) {
	s := NewServer(echo)

	_, offer := do(t, s, http.MethodGet, KeyPath, nil)
	env, _, _ := Seal(offer, http.MethodGet, "/v1/plugins", nil)
	var e envelope
	_ = json.Unmarshal(env, &e)
	e.Sealed[len(e.Sealed)-1] ^= 1
	bad, _ := json.Marshal(e)
	if code, _ := do(t, s, http.MethodPost, SealedPath, bad); code != http.StatusBadRequest {
		t.Errorf("tampered ciphertext: %d, want 400", code)
	}

	for _, path := range []string{SealedPath, KeyPath, "http://evil/v1/plugins", "/other"} {
		_, offer := do(t, s, http.MethodGet, KeyPath, nil)
		env, _, _ := Seal(offer, http.MethodGet, path, nil)
		if code, body := do(t, s, http.MethodPost, SealedPath, env); code != http.StatusBadRequest || !strings.Contains(string(body), "not allowed") {
			t.Errorf("inner path %q: %d %s, want refused", path, code, body)
		}
	}
	_, offer = do(t, s, http.MethodGet, KeyPath, nil)
	env, _, _ = Seal(offer, http.MethodPut, "/v1/plugins", nil)
	if code, _ := do(t, s, http.MethodPost, SealedPath, env); code != http.StatusBadRequest {
		t.Errorf("inner PUT: %d, want refused", code)
	}

	// Anything else reaches the routes as before (the SSH path).
	if code, body := do(t, s, http.MethodGet, "/v1/plugins", nil); code != http.StatusTeapot || !strings.HasPrefix(string(body), "GET /v1/plugins") {
		t.Errorf("plain route: %d %s", code, body)
	}
}

func TestSealRejectsABadOffer(t *testing.T) {
	for _, offer := range []string{``, `{}`, `{"id":"x","public_key":"AAAA"}`} {
		if _, _, err := Seal([]byte(offer), http.MethodGet, "/v1/plugins", nil); err == nil {
			t.Errorf("Seal accepted offer %q", offer)
		}
	}
}
