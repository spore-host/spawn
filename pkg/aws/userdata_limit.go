package aws

import (
	"encoding/base64"
	"fmt"
)

// EC2 refuses RunInstances with HTTP 400 when the user data is too large, and
// it applies two limits (spawn#1063):
//
//   - MaxUserDataBytes to the user data after base64 decoding, "User data is
//     limited to 16384 bytes". spawn gzips Linux user data before encoding it
//     (launcher.EncodeLinuxUserData, cmd encodeUserDataForOS), so this counts
//     gzip bytes; Windows user data is base64 of the plain script.
//   - MaxEncodedUserDataBytes to the base64 string itself, "Encoded User data
//     is limited to 25600 bytes".
//
// The first limit is the one that binds: 16384 decoded bytes encode to 21848
// characters. Both are checked anyway, because EC2 reports whichever it finds.
const (
	MaxUserDataBytes        = 16384
	MaxEncodedUserDataBytes = 25600
)

// UserDataTooLargeError is returned when a LaunchConfig's user data is over an
// EC2 limit. It is checked before any AWS call, so nothing has been created.
type UserDataTooLargeError struct {
	// DecodedBytes is the size EC2 checks against MaxUserDataBytes: the bytes
	// after base64 decoding, i.e. the gzip stream for Linux user data.
	DecodedBytes int
	// EncodedBytes is the length of the base64 string sent to RunInstances.
	EncodedBytes int
}

func (e *UserDataTooLargeError) Error() string {
	return fmt.Sprintf("user data is %d bytes after encoding (%d as sent, base64); EC2 allows %d (and %d base64), so RunInstances would refuse it",
		e.DecodedBytes, e.EncodedBytes, MaxUserDataBytes, MaxEncodedUserDataBytes)
}

// UserDataSize returns the two sizes EC2 limits for already-base64-encoded user
// data: the decoded byte count and the encoded length. A string that is not
// valid base64 is measured as if it were raw bytes, which is how much EC2 would
// have to accept; RunInstances rejects it for its encoding anyway (#127).
func UserDataSize(encoded string) (decoded, encodedLen int) {
	encodedLen = len(encoded)
	n, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return encodedLen, encodedLen
	}
	return len(n), encodedLen
}

// CheckUserDataSize returns a *UserDataTooLargeError when encoded (the exact
// string that would be sent as RunInstances UserData) is over either EC2 limit,
// and nil otherwise, including for empty user data.
func CheckUserDataSize(encoded string) error {
	if encoded == "" {
		return nil
	}
	decoded, encodedLen := UserDataSize(encoded)
	if decoded > MaxUserDataBytes || encodedLen > MaxEncodedUserDataBytes {
		return &UserDataTooLargeError{DecodedBytes: decoded, EncodedBytes: encodedLen}
	}
	return nil
}

// ValidateUserDataSize applies [CheckUserDataSize] to c.UserData. Client.Launch
// calls it before any AWS call, so every launch path (spawn launch, sweeps, job
// arrays, burst, pools, task run, launcher.Provision) refuses oversized user
// data before anything is created rather than at RunInstances.
func (c LaunchConfig) ValidateUserDataSize() error {
	return CheckUserDataSize(c.UserData)
}
