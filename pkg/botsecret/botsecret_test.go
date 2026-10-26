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

// A Slack signing secret is 32 hex characters; this has the same shape
// without being one.
const legacyPlaintext = "0123456789abcdef0123456789abcdef"

func TestPut_StoresCiphertextNeverPlaintext(t *testing.T) {
	ctx := context.Background()
	f := &fakeKMS{}
	item := map[string]types.AttributeValue{}
	if err := Put(ctx, f, "", item, "slack#T1", legacyPlaintext); err != nil {
		t.Fatal(err)
	}
	stored := Stored(item)
	if !security.IsEncrypted(stored) {
		t.Fatalf("stored signing_secret %q does not read as ciphertext", stored)
	}
	if strings.Contains(stored, legacyPlaintext) {
		t.Fatal("stored value contains the plaintext")
	}
	if f.encrypts != 1 {
		t.Fatalf("Encrypt calls = %d, want 1", f.encrypts)
	}
	// The ciphertext is bound to its purpose and to this item.
	for _, e := range f.sealed {
		if want := EncryptionContext("slack#T1"); !maps.Equal(e.ctx, want) {
			t.Fatalf("encryption context = %v, want %v", e.ctx, want)
		}
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
		t.Run(name, func(t *testing.T) {
			item := map[string]types.AttributeValue{}
			if err := Put(ctx, tc.kms, "", item, tc.key, legacyPlaintext); err == nil {
				t.Fatal("Put succeeded; want an error")
			}
			if _, set := item[attribute]; set {
				t.Fatalf("Put set signing_secret to %q after failing", Stored(item))
			}
		})
	}
}

func TestPut_EmptyStaysEmptyWithoutKMS(t *testing.T) {
	item := map[string]types.AttributeValue{}
	if err := Put(context.Background(), nil, "", item, "discord#G1", ""); err != nil {
		t.Fatal(err)
	}
	if got := Stored(item); got != "" {
		t.Fatalf("Stored = %q, want empty", got)
	}
}

func TestOpen_LegacyPlaintextPassesThrough(t *testing.T) {
	f := &fakeKMS{}
	got, err := Open(context.Background(), f, "slack#T1", legacyPlaintext)
	if err != nil || got != legacyPlaintext {
		t.Fatalf("Open(legacy) = %q, %v; want the plaintext", got, err)
	}
	if f.decrypts != 0 {
		t.Fatalf("Decrypt calls = %d for a plaintext value, want 0", f.decrypts)
	}
	// And without any KMS client at all.
	if got, err := Open(context.Background(), nil, "slack#T1", legacyPlaintext); err != nil || got != legacyPlaintext {
		t.Fatalf("Open(legacy, nil KMS) = %q, %v", got, err)
	}
}

func TestOpen_DecryptsSealedValue(t *testing.T) {
	ctx := context.Background()
	f := &fakeKMS{}
	sealed, err := Seal(ctx, f, "", "slack#T1", legacyPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(ctx, f, "slack#T1", sealed)
	if err != nil || got != legacyPlaintext {
		t.Fatalf("Open(sealed) = %q, %v", got, err)
	}
	// A ciphertext moved to another item does not open there.
	if _, err := Open(ctx, f, "slack#T2", sealed); err == nil {
		t.Fatal("Open under another workspace key succeeded")
	}
	// Sealed but no KMS client: an error, never the ciphertext as the secret.
	if got, err := Open(ctx, nil, "slack#T1", sealed); err == nil {
		t.Fatalf("Open(sealed, nil KMS) = %q, want an error", got)
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
