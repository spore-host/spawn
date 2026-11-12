package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/accesstoken"
)

// TestWebAuthHandler covers the :443 proxy access-token gate (#590): only a
// token a client registered, and only until it expires.
func TestWebAuthHandler(t *testing.T) {
	now := time.Now()
	store := &accesstoken.Store{Dir: t.TempDir(), Now: func() time.Time { return now }}
	token, _ := accesstoken.Mint()
	exp, err := store.Add(accesstoken.KindWeb, accesstoken.Hash(token), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dcvToken, _ := accesstoken.Mint()
	if _, err := store.Add(accesstoken.KindDCV, accesstoken.Hash(dcvToken), time.Hour); err != nil {
		t.Fatal(err)
	}
	// Backing "app" — records whether the proxy was reached.
	reached := false
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; w.WriteHeader(200) })
	h := webAuthHandler(app, store)

	withCookie := func(v string) *http.Request {
		req := httptest.NewRequest("GET", "/", nil)
		// A request cookie simulating a browser send (not a Set-Cookie); HttpOnly/Secure are response-cookie concerns.
		// nosemgrep
		req.AddCookie(&http.Cookie{Name: "spore_token", Value: v})
		return req
	}

	t.Run("no token → 403, app not reached", func(t *testing.T) {
		reached = false
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != http.StatusForbidden || reached {
			t.Errorf("code = %d reached = %v, want 403 and not reached", rec.Code, reached)
		}
	})

	t.Run("valid ?spore_token → cookie (expiring with the token) + redirect", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/lab?spore_token="+token, nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("code = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/lab" {
			t.Errorf("redirect Location = %q, want /lab (token stripped)", loc)
		}
		sc := rec.Header().Get("Set-Cookie")
		if sc == "" || !containsAll(sc, "spore_token="+token, "Secure", "HttpOnly", "Expires="+exp.UTC().Format(http.TimeFormat)) {
			t.Errorf("Set-Cookie = %q, want a secure httponly spore_token cookie expiring %s", sc, exp)
		}
	})

	t.Run("valid cookie → passes through", func(t *testing.T) {
		reached = false
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withCookie(token))
		if !reached || rec.Code != 200 {
			t.Errorf("valid cookie should reach the app (reached=%v code=%d)", reached, rec.Code)
		}
	})

	for name, v := range map[string]string{
		"wrong cookie":       "nope",
		"the hash as cookie": accesstoken.Hash(token),
		"a DCV token":        dcvToken,
	} {
		t.Run(name+" → 403", func(t *testing.T) {
			reached = false
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, withCookie(v))
			if reached || rec.Code != http.StatusForbidden {
				t.Errorf("must 403 (reached=%v code=%d)", reached, rec.Code)
			}
		})
	}

	t.Run("expired token in a cookie → 403", func(t *testing.T) {
		defer func(n time.Time) { now = n }(now)
		now = now.Add(2 * time.Hour)
		reached = false
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withCookie(token))
		if reached || rec.Code != http.StatusForbidden {
			t.Errorf("expired token must 403 (reached=%v code=%d)", reached, rec.Code)
		}
	})

	t.Run("nil store → gate disabled (pass-through)", func(t *testing.T) {
		reached = false
		rec := httptest.NewRecorder()
		webAuthHandler(app, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if !reached || rec.Code != 200 {
			t.Errorf("WebAuth=none should pass through (reached=%v code=%d)", reached, rec.Code)
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
