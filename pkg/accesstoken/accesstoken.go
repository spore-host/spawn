// Package accesstoken holds the access tokens that gate a spore's browser
// sessions: the DCV streaming session (:8443) and the web-app TLS proxy (:443).
//
// The token never leaves the machine that asked for it. The client (`spawn
// app`, `spawn connect`, the portal) mints a random token locally and sends
// only its SHA-256 to the instance, by running `spored access-token add` over
// SSM. spored keeps the hashes in a root-only directory on tmpfs and checks a
// presented token against them. So:
//
//   - nothing account-readable carries the token: not an EC2 tag, not user-data,
//     not SSM command history (which holds only the hash);
//   - every token has an expiry, enforced on every check;
//   - each client, on each connect, gets its own token, so a second machine
//     never needs the first one's, and rotation is simply connecting again;
//   - the hashes live on /run, so a stop/start or reboot revokes them all.
//
// Whoever may register a token is whoever may run ssm:SendCommand on the
// instance, which is already a root shell there: the gate adds no new
// authority, it only stops tags from being a second way in.
package accesstoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Kind is what a token opens.
type Kind string

const (
	// KindDCV opens the DCV streaming session (verified by spored's :8444
	// external-authenticator endpoint).
	KindDCV Kind = "dcv"
	// KindWeb opens the web-app TLS proxy on :443.
	KindWeb Kind = "web"
)

// ParseKind validates a kind named on a command line.
func ParseKind(s string) (Kind, error) {
	switch Kind(s) {
	case KindDCV, KindWeb:
		return Kind(s), nil
	}
	return "", fmt.Errorf("unknown access-token kind %q (want %q or %q)", s, KindDCV, KindWeb)
}

// DefaultDir is where spored keeps the registered hashes. /run is tmpfs, so a
// reboot or a stop/start clears it; hibernation keeps it, as it keeps the
// session.
const DefaultDir = "/run/spored/access-tokens"

// DefaultTTL is how long a token a client registers stays valid.
const DefaultTTL = 12 * time.Hour

// MaxTTL caps what `spored access-token add` accepts, so a token cannot be
// registered as effectively permanent.
const MaxTTL = 7 * 24 * time.Hour

// tokenBytes is the token's entropy: 256 bits.
const tokenBytes = 32

// Mint returns a fresh random token, hex-encoded (URL-safe as it is).
func Mint() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate access token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Hash is the hex SHA-256 of a token: the only form of it that travels to the
// instance.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ValidHash reports whether h is a lower-case hex SHA-256. It is what keeps a
// hash usable as a file name.
func ValidHash(h string) bool {
	if len(h) != sha256.Size*2 {
		return false
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// record is one registered token, stored as <dir>/<hash>.
type record struct {
	Kind    Kind      `json:"kind"`
	Expires time.Time `json:"expires"`
}

// Store is the on-instance set of registered token hashes.
type Store struct {
	Dir string
	// Now is the clock; nil means time.Now. Tests set it.
	Now func() time.Time
}

// NewStore is a Store on DefaultDir.
func NewStore() *Store { return &Store{Dir: DefaultDir} }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) dir() string {
	if s.Dir == "" {
		return DefaultDir
	}
	return s.Dir
}

// ensureDir creates the directory root-only, and refuses one that is not a
// plain directory (a symlink planted there would redirect the writes).
func (s *Store) ensureDir() error {
	d := s.dir()
	if err := os.MkdirAll(d, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", d, err)
	}
	fi, err := os.Lstat(d)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", d)
	}
	return os.Chmod(d, 0o700)
}

// Add registers hash for kind until now+ttl. ttl must be positive and at most
// MaxTTL.
func (s *Store) Add(kind Kind, hash string, ttl time.Duration) (time.Time, error) {
	if _, err := ParseKind(string(kind)); err != nil {
		return time.Time{}, err
	}
	if !ValidHash(hash) {
		return time.Time{}, errors.New("access-token hash must be 64 lower-case hex characters (a SHA-256)")
	}
	if ttl <= 0 || ttl > MaxTTL {
		return time.Time{}, fmt.Errorf("access-token ttl %s out of range (0, %s]", ttl, MaxTTL)
	}
	if err := s.ensureDir(); err != nil {
		return time.Time{}, err
	}
	exp := s.now().Add(ttl).UTC()
	data, err := json.Marshal(record{Kind: kind, Expires: exp})
	if err != nil {
		return time.Time{}, err
	}
	final := filepath.Join(s.dir(), hash)
	tmp, err := os.CreateTemp(s.dir(), ".add-*")
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return time.Time{}, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return time.Time{}, err
	}
	if err := tmp.Close(); err != nil {
		return time.Time{}, err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return time.Time{}, err
	}
	return exp, nil
}

// Verify reports whether token is registered for kind and unexpired, and when
// it expires. An expired record is removed.
func (s *Store) Verify(kind Kind, token string) (bool, time.Time) {
	if token == "" {
		return false, time.Time{}
	}
	h := Hash(token)
	p := filepath.Join(s.dir(), h)
	data, err := os.ReadFile(p) // #nosec G304 -- h is a hex SHA-256, so p stays inside dir
	if err != nil {
		return false, time.Time{}
	}
	var r record
	if err := json.Unmarshal(data, &r); err != nil {
		return false, time.Time{}
	}
	if !s.now().Before(r.Expires) {
		_ = os.Remove(p)
		return false, time.Time{}
	}
	if r.Kind != kind {
		return false, time.Time{}
	}
	return true, r.Expires
}

// Prune removes expired and unreadable records and returns how many remain.
func (s *Store) Prune() (int, error) {
	entries, err := os.ReadDir(s.dir())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	live := 0
	for _, e := range entries {
		name := e.Name()
		p := filepath.Join(s.dir(), name)
		if strings.HasPrefix(name, ".") || !ValidHash(name) {
			continue
		}
		data, err := os.ReadFile(p) // #nosec G304 -- name is a hex SHA-256 inside dir
		var r record
		if err != nil || json.Unmarshal(data, &r) != nil || !s.now().Before(r.Expires) {
			_ = os.Remove(p)
			continue
		}
		live++
	}
	return live, nil
}

// RevokeAll removes every registered token.
func (s *Store) RevokeAll() error {
	err := os.RemoveAll(s.dir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// SporedPath is where the bootstrap installs spored.
const SporedPath = "/usr/local/bin/spored"

// AddCommand is the shell command a client runs on the instance, over SSM, to
// register hash for kind. Every value in it is validated here and is shell-safe
// by construction (an enum, 64 hex characters and an integer), so it is the
// only thing SSM's command history ever records: the hash, never the token.
func AddCommand(kind Kind, hash string, ttl time.Duration) (string, error) {
	if _, err := ParseKind(string(kind)); err != nil {
		return "", err
	}
	if !ValidHash(hash) {
		return "", errors.New("access-token hash must be a lower-case hex SHA-256")
	}
	if ttl <= 0 || ttl > MaxTTL {
		return "", fmt.Errorf("access-token ttl %s out of range (0, %s]", ttl, MaxTTL)
	}
	return fmt.Sprintf("%s access-token add --kind %s --sha256 %s --ttl %ds", SporedPath, kind, hash, int64(ttl/time.Second)), nil
}

// WebParam is the query parameter (and cookie) the web proxy reads the token
// from.
const WebParam = "spore_token"

// DCVURL is the browser URL for a DCV session: the token rides the query
// string and the session id the fragment, per DCV's external-authentication
// spec. token == "" yields the URL without one (DCV then shows its own login).
func DCVURL(host, token, sessionID string) string {
	if token == "" {
		return fmt.Sprintf("https://%s:8443/#%s", host, sessionID)
	}
	return fmt.Sprintf("https://%s:8443/?authToken=%s#%s", host, token, sessionID)
}

// WebURL is the browser URL for a web app behind spored's :443 proxy.
func WebURL(host, token string) string {
	if token == "" {
		return fmt.Sprintf("https://%s/", host)
	}
	return fmt.Sprintf("https://%s/?%s=%s", host, WebParam, token)
}
