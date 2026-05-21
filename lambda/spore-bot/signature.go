package main

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"

	"github.com/fullsailor/pkcs7"
)

// AWS publishes multiple certificates for EC2 instance identity document signatures.
// Sources: https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/regions-certs.html
// The base64-encoded signature uses the DSA certificate; the RSA-2048 signature uses
// a newer certificate. We embed both and try each.

// awsEC2CertPEM is the original RSA-1024 certificate (used for the base64 signature).
const awsEC2CertPEM = `-----BEGIN CERTIFICATE-----
MIIDIjCCAougAwIBAgIJAKnL4UEDMN/FMA0GCSqGSIb3DQEBBQUAMGoxCzAJBgNV
BAYTAlVTMRMwEQYDVQQIEwpXYXNoaW5ndG9uMRAwDgYDVQQHEwdTZWF0dGxlMRgw
FgYDVQQKEw9BbWF6b24uY29tIEluYy4xGjAYBgNVBAMTEWVjMi5hbWF6b25hd3Mu
Y29tMB4XDTE0MDYwNTE0MjgwMloXDTI0MDYwNTE0MjgwMlowajELMAkGA1UEBhMC
VVMxEzARBgNVBAgTCldhc2hpbmd0b24xEDAOBgNVBAcTB1NlYXR0bGUxGDAWBgNV
BAoTD0FtYXpvbi5jb20gSW5jLjEaMBgGA1UEAxMRZWMyLmFtYXpvbmF3cy5jb20w
gZ8wDQYJKoZIhvcNAQEBBQADgY0AMIGJAoGBAIe9GN//SRK2knbjeSgkxpAkz7+6
halIkv8P9133jqLxf8v6ZYJ8BAkdx6tFB8gECU/UTPmWMFCNmEpynMNTSaFtMWDs
GcrGKx96O6smRd0gL5PbBJgNQENTvlMCQ/VOJHHoO8rL/y2GGqaO0DEMFohfHolB
0Dk0PKUVJV0h4qlXAgMBAAGjgc8wgcwwHQYDVR0OBBYEFIuybFHRn+p+uVANBCaM
5h0FqXtGMIGZBgNVHSMEgZEwgY6AFIuybFHRn+p+uVANBCaM5h0FqXtGoW6kbDBq
MQswCQYDVQQGEwJVUzETMBEGA1UECBMKV2FzaGluZ3RvbjEQMA4GA1UEBxMHU2Vh
dHRsZTEYMBYGA1UEChMPQW1hem9uLmNvbSBJbmMuMRowGAYDVQQDExFlYzIuYW1h
em9uYXdzLmNvbYIJAKnL4UEDMN/FMAwGA1UdEwQFMAMBAf8wDQYJKoZIhvcNAQEF
BQADgYEAFYcz1OgEhQBXIwIdsgCOS8vEtiJYF+j9uO6jz7VOmJqlec0CgCuJsvAt
sMoou1EcM6iI/sof+DYFWT/S6MoscKTpRcHQ7MPz1jWChbqZS5OHmMHBx7ybMcwk
qlEJ/kOCTVvRrPvQf0pRRqiM7mQhSdOSqsBSPPOYzUlT6h0=
-----END CERTIFICATE-----`

// awsEC2CertRSA2048PEM is the RSA-2048 certificate (newer, used for RSA-2048 signature).
const awsEC2CertRSA2048PEM = `-----BEGIN CERTIFICATE-----
MIICSzCCAbQCCQDtQvkVxRvK9TANBgkqhkiG9w0BAQsFADBqMQswCQYDVQQGEwJV
UzETMBEGA1UECBMKV2FzaGluZ3RvbjEQMA4GA1UEBxMHU2VhdHRsZTEYMBYGA1UE
ChMPQW1hem9uLmNvbSBJbmMuMRowGAYDVQQDExFlYzIuYW1hem9uYXdzLmNvbTAe
Fw0xOTAyMDMwMzAwMDZaFw0yOTAyMDIwMzAwMDZaMGoxCzAJBgNVBAYTAlVTMRMw
EQYDVQQIEwpXYXNoaW5ndG9uMRAwDgYDVQQHEwdTZWF0dGxlMRgwFgYDVQQKEw9B
bWF6b24uY29tIEluYy4xGjAYBgNVBAMTEWVjMi5hbWF6b25hd3MuY29tMIGfMA0G
CSqGSIb3DQEBAQUAA4GNADCBiQKBgQDpPCqBE4FTMIG1L8MuBE2bklf/UXi/RX0z
U5W9sF7VdQ0f5HKUIZ0rHPb9Aof7BPAM4MXX4GPM6H8yHxBW3l3kkgBQPnRMZHiI
PStBZJ1R7yg9eZVNkXbVkmLGBd+HEsDBhGWVnHPMZlmA7JJ56/fT5vWjzRBxV7vb
A7qeKQIDAQABMA0GCSqGSIb3DQEBCwUAA4GBAIcHEHqmFM3qGDolMnpW6ZuYarAx
hWdlzSaLTm3drnB6wnqMnMn0gQ5KoLc4QUVAKMnvZjY2CqBCNMBVW9Zl5q5UGk3
8b8+6UcGEzOiqZJpDqJxGVBJZVNXqJVnThDuQy+wKoTrYHtFkVGb2P9HiPJLPiGR
u+KxJEnW5WtF1Z6l
-----END CERTIFICATE-----`

// verifyNotifyAuth verifies a NotifyRequest's instance identity.
// Prefers PKCS#7 (self-contained, no hardcoded certs) over legacy doc+signature.
func verifyNotifyAuth(nr NotifyRequest) error {
	if nr.PKCS7 != "" {
		pkcs7Bytes, err := base64.StdEncoding.DecodeString(nr.PKCS7)
		if err != nil {
			return fmt.Errorf("decode pkcs7: %w", err)
		}
		return verifyPKCS7(nil, pkcs7Bytes)
	}

	// Legacy path: separate document + signature
	if nr.InstanceIdentityDocument == "" {
		return fmt.Errorf("missing instance identity")
	}
	docBytes, err := base64.StdEncoding.DecodeString(nr.InstanceIdentityDocument)
	if err != nil {
		return fmt.Errorf("decode document: %w", err)
	}
	return verifyInstanceIdentitySignature(docBytes, nr.InstanceIdentitySignature)
}

// verifyInstanceIdentitySignature verifies the RSA signature on an EC2 instance
// identity document using AWS's published certificates.
func verifyInstanceIdentitySignature(docBytes []byte, signatureB64 string) error {
	if signatureB64 == "" {
		return fmt.Errorf("instance identity signature is required")
	}
	sigBytes, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	certs, err := parseAWSCerts()
	if err != nil {
		return fmt.Errorf("parse AWS certificates: %w", err)
	}
	for _, cert := range certs {
		if err := verifyRawRSA(docBytes, sigBytes, cert); err == nil {
			return nil
		}
	}
	return fmt.Errorf("signature verification failed")
}

func parseAWSCerts() ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for _, pemStr := range []string{awsEC2CertPEM, awsEC2CertRSA2048PEM} {
		block, _ := pem.Decode([]byte(pemStr))
		if block == nil {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no valid certificates found")
	}
	return certs, nil
}

// verifyPKCS7 verifies a PKCS#7 envelope. When docBytes is nil the document
// is taken from the envelope itself (the /instance-identity/pkcs7 endpoint
// returns a self-contained signed message with content embedded).
func verifyPKCS7(docBytes, sigBytes []byte) error {
	p7, err := pkcs7.Parse(sigBytes)
	if err != nil {
		return fmt.Errorf("parse PKCS#7: %w", err)
	}
	if docBytes != nil {
		p7.Content = docBytes
	}
	return p7.Verify()
}

func verifyRawRSA(docBytes, sigBytes []byte, cert *x509.Certificate) error {
	rsaPub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("certificate does not contain RSA public key")
	}
	h := sha256.Sum256(docBytes)
	return rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, h[:], sigBytes)
}
