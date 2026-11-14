package agent

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captureLog records what the standard logger writes for the test's duration.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return &buf
}

const secretPath = "/services/T000/B000/s3cr3t-t0ken"

// A webhook URL is the credential, so no outcome of a POST may log it (#1592):
// not success, not a non-2xx, not a transport error (whose *url.Error quotes
// the URL in full), not a URL that fails to build a request.
func TestPostWebhookNeverLogsTheURL(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer bad.Close()
	// A server that is closed before the POST: a transport error.
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	goneURL := gone.URL
	gone.Close()

	for _, tc := range []struct {
		name, url, want string
	}{
		{"success", ok.URL + secretPath + "?token=q-s3cr3t", "notice POSTed to " + ok.URL},
		{"non-2xx", bad.URL + secretPath + "?token=q-s3cr3t", "returned 403"},
		{"transport error", goneURL + secretPath + "?token=q-s3cr3t", "POST to " + goneURL + " failed"},
		{"unbuildable", "https://hooks.example" + secretPath + "?token=q-s3cr3t\x7f", "request build for"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLog(t)
			postWebhook("Test webhook", tc.url, 2*time.Second, map[string]string{"event": "x"})
			got := buf.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("log %q does not contain %q", got, tc.want)
			}
			for _, secret := range []string{"s3cr3t-t0ken", "q-s3cr3t", "/services/"} {
				if strings.Contains(got, secret) {
					t.Errorf("log leaks %q: %q", secret, got)
				}
			}
		})
	}
}

func TestWebhookEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"https://hooks.slack.com/services/T/B/x":     "https://hooks.slack.com",
		"https://user:pass@example.com:8443/p?q=1#f": "https://example.com:8443",
		"not a url":      "webhook",
		"":               "webhook",
		"/relative/path": "webhook",
	} {
		if got := webhookEndpoint(in); got != want {
			t.Errorf("webhookEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
