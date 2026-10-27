package botsecret

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/spore-host/spawn/pkg/security"
)

// fakeKMS behaves like KMS where it matters here: the blob is opaque (it does
// not contain the plaintext), is long enough to be a real ciphertext, and
// decrypts only with the encryption context it was made with.
type fakeKMS struct {
	encrypts, decrypts int
	sealed             map[string]sealedEntry
	shortBlob          bool // return a blob too short to read as ciphertext
}

type sealedEntry struct {
	plaintext []byte
	ctx       map[string]string
}

func (f *fakeKMS) Encrypt(_ context.Context, in *kms.EncryptInput, _ ...func(*kms.Options)) (*kms.EncryptOutput, error) {
	f.encrypts++
	if f.sealed == nil {
		f.sealed = map[string]sealedEntry{}
	}
	blob := bytes.Repeat([]byte{byte(0xA0 + len(f.sealed))}, 180)
	if f.shortBlob {
		blob = []byte("short")
	}
	f.sealed[string(blob)] = sealedEntry{plaintext: in.Plaintext, ctx: maps.Clone(in.EncryptionContext)}
	return &kms.EncryptOutput{CiphertextBlob: blob}, nil
}

func (f *fakeKMS) Decrypt(_ context.Context, in *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	f.decrypts++
	e, ok := f.sealed[string(in.CiphertextBlob)]
	if !ok {
		return nil, errors.New("InvalidCiphertextException")
	}
	if !maps.Equal(e.ctx, in.EncryptionContext) {
		return nil, errors.New("InvalidCiphertextException: encryption context mismatch")
	}
	return &kms.DecryptOutput{Plaintext: e.plaintext}, nil
}

var _ security.KMSAPI = (*fakeKMS)(nil)

// Legacy (pre-#868) plaintext shapes, one per field, built or worded so secret
// scanners do not flag them: a Slack signing secret is 32 hex characters
// (valid base64, but short), Slack tokens carry '-' (not base64), a webhook URL
// carries "://".
var legacyPlaintexts = map[Field]string{
	SigningSecret:      strings.Repeat("0123456789abcdef", 2),
	BotToken:           "xoxb-not-a-real-bot-token-fixture",
	RefreshToken:       "xoxe-1-not-a-real-refresh-token-fixture",
	IncomingWebhookURL: "https://hooks.example.invalid/not-a-real-webhook-fixture",
}

func TestFieldsHaveDistinctPurposesAndFixtures(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range Fields() {
		if seen[f.Purpose()] || seen[f.attribute] {
			t.Fatalf("%s reuses a purpose or attribute", f)
		}
		seen[f.Purpose()], seen[f.attribute] = true, true
		if _, ok := legacyPlaintexts[f]; !ok {
			t.Fatalf("no legacy fixture for %s", f)
		}
	}
}

func TestPut_StoresCiphertextNeverPlaintext(t *testing.T) {
	ctx := context.Background()
	for _, fld := range Fields() {
		t.Run(fld.String(), func(t *testing.T) {
			f := &fakeKMS{}
			plain := legacyPlaintexts[fld]
			item := map[string]types.AttributeValue{}
			if err := fld.Put(ctx, f, "", item, "slack#T1", plain); err != nil {
				t.Fatal(err)
			}
			stored := fld.Stored(item)
			if !security.IsEncrypted(stored) {
				t.Fatalf("stored %s %q does not read as ciphertext", fld, stored)
			}
			if strings.Contains(stored, plain) {
				t.Fatal("stored value contains the plaintext")
			}
			if len(item) != 1 {
				t.Fatalf("Put wrote %d attributes, want 1", len(item))
			}
			if f.encrypts != 1 {
				t.Fatalf("Encrypt calls = %d, want 1", f.encrypts)
			}
			// The ciphertext is bound to its purpose and to this item.
			for _, e := range f.sealed {
				if want := fld.EncryptionContext("slack#T1"); !maps.Equal(e.ctx, want) || e.ctx["purpose"] != fld.Purpose() {
					t.Fatalf("encryption context = %v, want %v", e.ctx, want)
				}
			}
		})
	}
}

func TestPut_FailsClosed(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		kms security.KMSAPI
		key string
	}{
		"no KMS client":                {kms: nil, key: "slack#T1"},
		"KMS blob not ciphertext-like": {kms: &fakeKMS{shortBlob: true}, key: "slack#T1"},
		"no workspace key":             {kms: &fakeKMS{}, key: ""},
	}
	for name, tc := range cases {
		for _, fld := range Fields() {
			t.Run(name+"/"+fld.String(), func(t *testing.T) {
				item := map[string]types.AttributeValue{}
				if err := fld.Put(ctx, tc.kms, "", item, tc.key, legacyPlaintexts[fld]); err == nil {
					t.Fatal("Put succeeded; want an error")
				}
				if _, set := item[fld.attribute]; set {
					t.Fatalf("Put set %s to %q after failing", fld.attribute, fld.Stored(item))
				}
			})
		}
	}
}

func TestPut_EmptyRemovesAttributeWithoutKMS(t *testing.T) {
	for _, fld := range Fields() {
		item := map[string]types.AttributeValue{fld.attribute: &types.AttributeValueMemberS{Value: "old"}}
		if err := fld.Put(context.Background(), nil, "", item, "discord#G1", ""); err != nil {
			t.Fatal(err)
		}
		if _, set := item[fld.attribute]; set {
			t.Fatalf("%s: empty Put left the attribute: %v", fld, item)
		}
	}
}

func TestOpen_LegacyPlaintextPassesThrough(t *testing.T) {
	for _, fld := range Fields() {
		plain := legacyPlaintexts[fld]
		f := &fakeKMS{}
		got, err := fld.Open(context.Background(), f, "slack#T1", plain)
		if err != nil || got != plain {
			t.Fatalf("%s: Open(legacy) = %q, %v; want the plaintext", fld, got, err)
		}
		if f.decrypts != 0 {
			t.Fatalf("%s: Decrypt calls = %d for a plaintext value, want 0", fld, f.decrypts)
		}
		// And without any KMS client at all.
		if got, err := fld.Open(context.Background(), nil, "slack#T1", plain); err != nil || got != plain {
			t.Fatalf("%s: Open(legacy, nil KMS) = %q, %v", fld, got, err)
		}
	}
}

func TestOpen_DecryptsSealedValue(t *testing.T) {
	ctx := context.Background()
	for _, fld := range Fields() {
		t.Run(fld.String(), func(t *testing.T) {
			f := &fakeKMS{}
			plain := legacyPlaintexts[fld]
			sealed, err := fld.Seal(ctx, f, "", "slack#T1", plain)
			if err != nil {
				t.Fatal(err)
			}
			got, err := fld.Open(ctx, f, "slack#T1", sealed)
			if err != nil || got != plain {
				t.Fatalf("Open(sealed) = %q, %v", got, err)
			}
			// A ciphertext moved to another item does not open there.
			if _, err := fld.Open(ctx, f, "slack#T2", sealed); err == nil {
				t.Fatal("Open under another workspace key succeeded")
			}
			// Nor as another field.
			for _, other := range Fields() {
				if other != fld {
					if _, err := other.Open(ctx, f, "slack#T1", sealed); err == nil {
						t.Fatalf("a sealed %s opened as %s", fld, other)
					}
				}
			}
			// Sealed but no KMS client: an error, never the ciphertext as the value.
			if got, err := fld.Open(ctx, nil, "slack#T1", sealed); err == nil {
				t.Fatalf("Open(sealed, nil KMS) = %q, want an error", got)
			}
		})
	}
}

func TestRotatedTokens_SealsBoth(t *testing.T) {
	ctx := context.Background()
	f := &fakeKMS{}
	bot, refresh := legacyPlaintexts[BotToken], legacyPlaintexts[RefreshToken]
	update, values, err := RotatedTokens(ctx, f, "", "slack#T1", bot, refresh, 1234)
	if err != nil {
		t.Fatal(err)
	}
	want := "SET bot_token = :t, refresh_token = :r, token_expires_at = :e"
	if update != want {
		t.Fatalf("update = %q, want %q", update, want)
	}
	for ph, fld := range map[string]Field{":t": BotToken, ":r": RefreshToken} {
		v := values[ph].(*types.AttributeValueMemberS).Value
		if !security.IsEncrypted(v) || strings.Contains(v, legacyPlaintexts[fld]) {
			t.Fatalf("%s value %q is not sealed", ph, v)
		}
		if got, err := fld.Open(ctx, f, "slack#T1", v); err != nil || got != legacyPlaintexts[fld] {
			t.Fatalf("%s opens to %q, %v", ph, got, err)
		}
	}
	if values[":e"].(*types.AttributeValueMemberN).Value != "1234" {
		t.Fatalf(":e = %v", values[":e"])
	}
	if _, _, err := RotatedTokens(ctx, nil, "", "slack#T1", bot, refresh, 1); err == nil {
		t.Fatal("RotatedTokens without KMS succeeded")
	}
	// No new refresh token: the old one is removed, not left stale or set to "".
	update, values, err = RotatedTokens(ctx, f, "", "slack#T1", bot, "", 1)
	if err != nil || !strings.HasSuffix(update, " REMOVE refresh_token") || values[":r"] != nil {
		t.Fatalf("update = %q, values = %v, err = %v", update, values, err)
	}
}

func TestKeyID(t *testing.T) {
	t.Setenv(EnvKeyID, "")
	if got := KeyID(); got != DefaultKeyAlias {
		t.Fatalf("KeyID() = %q, want %q", got, DefaultKeyAlias)
	}
	t.Setenv(EnvKeyID, "alias/other")
	if got := KeyID(); got != "alias/other" {
		t.Fatalf("KeyID() = %q with %s set", got, EnvKeyID)
	}
}

// The fake's blob must read as ciphertext, or the tests above prove nothing.
func TestFakeBlobReadsAsCiphertext(t *testing.T) {
	if !security.IsEncrypted(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xA0}, 180))) {
		t.Fatal("fake ciphertext is not recognised by IsEncrypted")
	}
}
