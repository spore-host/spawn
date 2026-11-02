package main

import (
	"crypto/sha1" //nolint:gosec // RFC 4880 v4 fingerprints are SHA-1 by definition
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// keyFingerprint returns the v4 fingerprint of the primary key in an
// ASCII-armored OpenPGP public key: SHA-1 over 0x99, the body's two-byte
// length and the public-key packet body (RFC 4880 section 12.2). It is what
// `gpg --fingerprint` prints, computed without gpg so the test runs anywhere.
func keyFingerprint(armored string) (string, error) {
	body, err := dearmor(armored)
	if err != nil {
		return "", err
	}
	tag, pkt, err := firstPacket(body)
	if err != nil {
		return "", err
	}
	if tag != 6 {
		return "", fmt.Errorf("first packet is tag %d, not a public key (6)", tag)
	}
	if len(pkt) == 0 || pkt[0] != 4 {
		return "", errors.New("not a version 4 public key")
	}
	if len(pkt) > 0xffff {
		return "", errors.New("public-key packet too long for a v4 fingerprint")
	}
	h := sha1.New() //nolint:gosec // see the import
	h.Write([]byte{0x99, byte(len(pkt) >> 8), byte(len(pkt))})
	h.Write(pkt)
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}

// dearmor returns the binary inside a PGP PUBLIC KEY BLOCK: the base64 after
// the armor headers' terminating blank line, without the CRC-24 line.
func dearmor(armored string) ([]byte, error) {
	const begin, end = "-----BEGIN PGP PUBLIC KEY BLOCK-----", "-----END PGP PUBLIC KEY BLOCK-----"
	s := strings.ReplaceAll(armored, "\r", "")
	i, j := strings.Index(s, begin), strings.Index(s, end)
	if i < 0 || j < i {
		return nil, errors.New("not an ASCII-armored PGP public key block")
	}
	inner := strings.TrimPrefix(s[i+len(begin):j], "\n")
	_, data, ok := strings.Cut(inner, "\n\n")
	if !ok {
		data = inner // no blank line at all: no headers either
	}
	if strings.HasPrefix(inner, "\n") {
		data = inner[1:] // a blank line straight after BEGIN: no headers
	}
	var b64 strings.Builder
	for _, l := range strings.Split(data, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "=") {
			continue
		}
		b64.WriteString(l)
	}
	return base64.StdEncoding.DecodeString(b64.String())
}

// firstPacket parses the first OpenPGP packet header (old or new format, RFC
// 4880 section 4.2) and returns its tag and body.
func firstPacket(b []byte) (int, []byte, error) {
	if len(b) < 2 || b[0]&0x80 == 0 {
		return 0, nil, errors.New("not an OpenPGP packet")
	}
	var tag, n, hdr int
	if b[0]&0x40 != 0 { // new format
		tag = int(b[0] & 0x3f)
		switch o := b[1]; {
		case o < 192:
			n, hdr = int(o), 2
		case o < 224:
			if len(b) < 3 {
				return 0, nil, errors.New("truncated packet header")
			}
			n, hdr = (int(o)-192)<<8+int(b[2])+192, 3
		case o == 255:
			if len(b) < 6 {
				return 0, nil, errors.New("truncated packet header")
			}
			n, hdr = int(b[2])<<24|int(b[3])<<16|int(b[4])<<8|int(b[5]), 6
		default:
			return 0, nil, errors.New("partial-length public-key packet")
		}
	} else { // old format
		tag = int(b[0]>>2) & 0x0f
		switch b[0] & 3 {
		case 0:
			n, hdr = int(b[1]), 2
		case 1:
			if len(b) < 3 {
				return 0, nil, errors.New("truncated packet header")
			}
			n, hdr = int(b[1])<<8|int(b[2]), 3
		case 2:
			if len(b) < 5 {
				return 0, nil, errors.New("truncated packet header")
			}
			n, hdr = int(b[1])<<24|int(b[2])<<16|int(b[3])<<8|int(b[4]), 5
		default:
			return 0, nil, errors.New("indeterminate-length public-key packet")
		}
	}
	if len(b) < hdr+n {
		return 0, nil, errors.New("truncated packet")
	}
	return tag, b[hdr : hdr+n], nil
}
