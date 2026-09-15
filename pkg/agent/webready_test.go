package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildWebReadyURL(t *testing.T) {
	if got := buildWebReadyURL("box.5k0zfnmq.spore.host", ""); got != "https://box.5k0zfnmq.spore.host/" {
		t.Errorf("no-token: buildWebReadyURL = %q", got)
	}
	if got := buildWebReadyURL("box.5k0zfnmq.spore.host", "deadbeef"); got != "https://box.5k0zfnmq.spore.host/?spore_token=deadbeef" {
		t.Errorf("token: buildWebReadyURL = %q", got)
	}
}

// TestWebAuthHandler covers the :443 proxy access-token gate (#590).
func TestWebAuthHandler(t *testing.T) {
	const token = "s3cr3ttoken"
	// Backing "app" — records whether the proxy was reached.
	reached := false
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; w.WriteHeader(200) })

	t.Run("no token → 403, app not reached", func(t *testing.T) {
		reached = false
		h := webAuthHandler(app, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("code = %d, want 403", rec.Code)
		}
		if reached {
			t.Error("app must not be reached without a token")
		}
	})

	t.Run("valid ?spore_token → cookie + redirect", func(t *testing.T) {
		h := webAuthHandler(app, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/lab?spore_token="+token, nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("code = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/lab" {
			t.Errorf("redirect Location = %q, want /lab (token stripped)", loc)
		}
		sc := rec.Header().Get("Set-Cookie")
		if sc == "" || !containsAll(sc, "spore_token="+token, "Secure", "HttpOnly") {
			t.Errorf("Set-Cookie = %q, want secure httponly spore_token cookie", sc)
		}
	})

	t.Run("valid cookie → passes through", func(t *testing.T) {
		reached = false
		h := webAuthHandler(app, token)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: "spore_token", Value: token})
		h.ServeHTTP(rec, req)
		if !reached || rec.Code != 200 {
			t.Errorf("valid cookie should reach the app (reached=%v code=%d)", reached, rec.Code)
		}
	})

	t.Run("wrong cookie → 403", func(t *testing.T) {
		reached = false
		h := webAuthHandler(app, token)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: "spore_token", Value: "nope"})
		h.ServeHTTP(rec, req)
		if reached || rec.Code != http.StatusForbidden {
			t.Errorf("wrong cookie must 403 (reached=%v code=%d)", reached, rec.Code)
		}
	})

	t.Run("empty token → gate disabled (pass-through)", func(t *testing.T) {
		reached = false
		h := webAuthHandler(app, "")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if !reached || rec.Code != 200 {
			t.Errorf("empty token should pass through (reached=%v code=%d)", reached, rec.Code)
		}
	})
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestProbeHTTP_Down confirms the probe reports "not up" when nothing is
// listening (a closed port). The "up" case is covered end-to-end by the
// real-instance validation, since it needs a live server.
func TestProbeHTTP_Down(t *testing.T) {
	// Port 1 is reserved and won't be listening in the test environment.
	if probeHTTP(1, "/") {
		t.Error("probeHTTP should report down for a port with no listener")
	}
}
