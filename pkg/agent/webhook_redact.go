package agent

import (
	"errors"
	"net/url"
)

// A webhook URL is itself the credential: Slack, Teams and most queue
// endpoints authorise a POST by the token in its path or query. So spored
// never logs one (#1592). webhookEndpoint is what it logs instead, and
// redactURLError strips the URL from the errors net/http returns, which
// otherwise quote it in full.

// webhookEndpoint is raw's scheme and host, or "webhook" when raw does not
// parse as an absolute URL. Never the path, query or user info.
func webhookEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "webhook"
	}
	return u.Scheme + "://" + u.Host
}

// redactURLError returns err with every *url.Error's URL replaced by its
// scheme and host. http.Client.Do and http.NewRequest wrap their failures in
// one, whose message is `<Op> "<full URL>": <cause>`.
func redactURLError(err error) error {
	var ue *url.Error
	if err == nil || !errors.As(err, &ue) {
		return err
	}
	return &url.Error{Op: ue.Op, URL: webhookEndpoint(ue.URL), Err: redactURLError(ue.Err)}
}
