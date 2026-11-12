package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/accesstoken"
	"github.com/spore-host/spawn/pkg/provider"
)

// registerTestToken registers a fresh token on a's store, as `spored
// access-token add` does for a client, and returns it.
func registerTestToken(t *testing.T, a *Agent, kind accesstoken.Kind) string {
	t.Helper()
	tok, err := accesstoken.Mint()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.tokenStore().Add(kind, accesstoken.Hash(tok), time.Hour); err != nil {
		t.Fatal(err)
	}
	return tok
}

// assertNoTokenPublished is the owner's rule for session tokens: never in an
// EC2 tag. No write may carry spawn:ready-token, and no ready-url may carry a
// token parameter.
func assertNoTokenPublished(t *testing.T, tagger *fakeTagPutter) {
	t.Helper()
	for _, w := range tagger.writes {
		if _, ok := w[legacyReadyTokenTag]; ok {
			t.Errorf("a tag write published %s: %v", legacyReadyTokenTag, w)
		}
		if u := w["spawn:ready-url"]; strings.Contains(u, "authToken=") || strings.Contains(u, accesstoken.WebParam+"=") {
			t.Errorf("spawn:ready-url carries a token: %q", u)
		}
	}
}

func dcvVerify(t *testing.T, h http.Handler, sessionID, token string) bool {
	t.Helper()
	form := url.Values{"sessionId": {sessionID}, "authenticationToken": {token}, "clientAddress": {"203.0.113.9"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	yes := strings.Contains(body, `result="yes"`)
	if yes && !strings.Contains(body, "<username>"+dcvUsername+"</username>") {
		t.Errorf("accepted without the username: %s", body)
	}
	return yes
}

// The DCV verifier accepts only a registered, unexpired, DCV-kind token, and
// only for this instance's session.
func TestDCVAuthHandler(t *testing.T) {
	now := time.Now()
	store := &accesstoken.Store{Dir: t.TempDir(), Now: func() time.Time { return now }}
	h := dcvAuthHandler(store, "console")

	tok, _ := accesstoken.Mint()
	if _, err := store.Add(accesstoken.KindDCV, accesstoken.Hash(tok), time.Hour); err != nil {
		t.Fatal(err)
	}
	webTok, _ := accesstoken.Mint()
	if _, err := store.Add(accesstoken.KindWeb, accesstoken.Hash(webTok), time.Hour); err != nil {
		t.Fatal(err)
	}

	if !dcvVerify(t, h, "console", tok) {
		t.Error("a registered token was refused")
	}
	if !dcvVerify(t, h, "console", tok) {
		t.Error("a registered token was refused on reconnect, before it expired")
	}
	if dcvVerify(t, h, "console", "deadbeef") {
		t.Error("an unregistered token was accepted")
	}
	if dcvVerify(t, h, "console", accesstoken.Hash(tok)) {
		t.Error("the hash itself (what SSM history holds) was accepted as a token")
	}
	if dcvVerify(t, h, "other", tok) {
		t.Error("a token was accepted for a session other than this instance's")
	}
	if dcvVerify(t, h, "console", webTok) {
		t.Error("a web-proxy token opened the DCV session")
	}
	if dcvVerify(t, h, "console", "") {
		t.Error("an empty token was accepted")
	}
	now = now.Add(2 * time.Hour)
	if dcvVerify(t, h, "console", tok) {
		t.Error("an expired token was accepted")
	}
}

// Neither handshake publishes a token, and each says how to get one.
func TestReadyHandshakesPublishNoToken(t *testing.T) {
	ctx := context.Background()

	t.Run("dcv", func(t *testing.T) {
		tagger := &fakeTagPutter{}
		a := dcvAuthAgent(t, &fakeDCVRunner{isInstalled: true, listOut: "Session: 'console'"}, tagger)
		a.maybeSetupDCVAuth(ctx)
		if tagger.tags["spawn:ready-status"] != string(dcvReady) || tagger.tags[readyAuthTag] != readyAuthToken {
			t.Fatalf("ready write = %v", tagger.tags)
		}
		if want := "https://1.2.3.4:8443/#console"; tagger.tags["spawn:ready-url"] != want {
			t.Errorf("ready-url = %q, want %q", tagger.tags["spawn:ready-url"], want)
		}
		assertNoTokenPublished(t, tagger)
	})

	for _, tc := range []struct {
		webAuth, wantAuth string
	}{{"token", readyAuthToken}, {"", readyAuthToken}, {"none", readyAuthNone}} {
		t.Run("web auth="+tc.webAuth, func(t *testing.T) {
			app := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			t.Cleanup(app.Close)
			port, _ := strconv.Atoi(app.URL[strings.LastIndex(app.URL, ":")+1:])
			a := newTestAgent(t, &provider.Config{AppMode: "web", ReadyPort: port, WebAuth: tc.webAuth})
			a.identity.Provider = "ec2"
			a.webProxyStarted = true
			tagger := &fakeTagPutter{}
			a.tagger = tagger
			a.maybeSetupWebReady(ctx)
			if !a.webReadyDone || tagger.tags[readyAuthTag] != tc.wantAuth {
				t.Fatalf("done=%v tags=%v, want %s=%s", a.webReadyDone, tagger.tags, readyAuthTag, tc.wantAuth)
			}
			assertNoTokenPublished(t, tagger)
		})
	}
}

// On its first tick a readiness-reporting spored deletes the tags an older one
// published its token in, and the readiness of the previous start.
func TestClearStaleReadyTags(t *testing.T) {
	ctx := context.Background()
	stale := []string{legacyReadyTokenTag, "spawn:ready-url", "spawn:ready-status", readyAuthTag}

	t.Run("dcv instance: all stale keys, once", func(t *testing.T) {
		tagger := &fakeTagPutter{tags: map[string]string{legacyReadyTokenTag: "old", "spawn:ready-url": "https://h:8443/?authToken=old#console"}}
		a := dcvAuthAgent(t, &fakeDCVRunner{isInstalled: true, listOut: "no sessions yet"}, tagger)
		a.clearStaleReadyTags(ctx)
		a.clearStaleReadyTags(ctx)
		if len(tagger.deletes) != 1 || !slices.Equal(tagger.deletes[0], stale) {
			t.Fatalf("deletes = %v, want one call deleting %v", tagger.deletes, stale)
		}
		if _, ok := tagger.tags[legacyReadyTokenTag]; ok {
			t.Error("the legacy token tag survived")
		}
	})

	t.Run("failed delete retried; this start's ready-url kept", func(t *testing.T) {
		tagger := &fakeTagPutter{deleteErr: errors.New("throttled")}
		a := dcvAuthAgent(t, &fakeDCVRunner{isInstalled: true, listOut: "Session: 'console'"}, tagger)
		a.clearStaleReadyTags(ctx)
		a.maybeSetupDCVAuth(ctx) // this start reports ready meanwhile
		tagger.deleteErr = nil
		a.clearStaleReadyTags(ctx)
		last := tagger.deletes[len(tagger.deletes)-1]
		if !slices.Equal(last, []string{legacyReadyTokenTag}) {
			t.Errorf("retry deleted %v; once this start is ready only %s may go", last, legacyReadyTokenTag)
		}
		if tagger.tags["spawn:ready-url"] == "" {
			t.Error("the retry deleted this start's ready-url")
		}
	})

	t.Run("not a session instance: nothing deleted", func(t *testing.T) {
		tagger := &fakeTagPutter{}
		a := newTestAgent(t, nil)
		a.identity.Provider = "ec2"
		a.tagger = tagger
		a.clearStaleReadyTags(ctx)
		if len(tagger.deletes) != 0 {
			t.Errorf("deleted %v on an instance with no session", tagger.deletes)
		}
	})

	// Through the monitor loop, so the call site is covered too.
	t.Run("first tick", func(t *testing.T) {
		tagger := &fakeTagPutter{}
		a := dcvAuthAgent(t, &fakeDCVRunner{isInstalled: true, listOut: "no sessions yet"}, tagger)
		hermeticTicks(a)
		a.checkAndAct(ctx)
		if len(tagger.deletes) == 0 || !slices.Contains(tagger.deletes[0], legacyReadyTokenTag) {
			t.Errorf("the first tick did not delete %s (deletes %v)", legacyReadyTokenTag, tagger.deletes)
		}
	})
}
