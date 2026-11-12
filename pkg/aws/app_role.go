package aws

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spore-host/spawn/pkg/dns"
)

// The app role: the only spawn role that may read the account's wildcard TLS
// key.
//
// `spawn app launch` (cmd/app.go) installs the account's wildcard certificate
// for *.<account base36>.spore.host from s3://spawn-certs-<region>/<base36>/
// {cert,key}.pem, for spored's :443 web proxy and for the DCV server. The read
// used to be on spored-instance-role, which every launch without IAM flags
// shares, so any process on any default spore could read the key and, with a
// DNS record pointed at itself, serve a valid certificate for any name in the
// account.
//
// So app launches get a role of their own, and spored-instance-role carries an
// explicit Deny of the read (its policy schema 3): the Deny, because CLIs
// before #1558 still rewrite the old unversioned spored-policy with the grant.
//
// AppRoleName is fixed rather than a hash of its policy so the certificate
// bucket's policy, in the infra account, can name it (as it names
// spored-instance-role today). It is in the spawn-instance-* family, so
// pkg/instancerole (the TTL reaper's role sweep and `spawn cleanup
// --instance-roles`) is its deleter: tagged spawn:managed=true, no live
// instance on the profile, a day old and unused for a day. The next app launch
// recreates it.
const AppRoleName = "spawn-instance-app"

// appRolePolicyDoc is the app role's inline grants beyond the spored baseline
// (which every spawn-instance-* role gets): what the app user data and spored
// read that the baseline does not grant. accountID binds the certificate read
// to the account's own prefix, so the role cannot read another account's key
// even where a bucket policy would allow it.
func appRolePolicyDoc(accountID string) (string, error) {
	if !accountIDPattern.MatchString(accountID) {
		return "", fmt.Errorf("cannot scope the app role's certificate read to account %q: not a 12-digit account ID", accountID)
	}
	b36 := dns.EncodeAccountID(accountID)
	doc := map[string]interface{}{
		"Version": "2012-10-17",
		"Statement": []interface{}{
			map[string]interface{}{
				"Sid":      "AccountWildcardCertificate",
				"Effect":   "Allow",
				"Action":   "s3:GetObject",
				"Resource": "arn:aws:s3:::spawn-certs-*/" + b36 + "/*",
			},
			// The DCV license check on EC2 (an AWS-owned bucket per region).
			map[string]interface{}{
				"Sid":      "DCVLicense",
				"Effect":   "Allow",
				"Action":   "s3:GetObject",
				"Resource": "arn:aws:s3:::dcv-license.*/*",
			},
			// A private-ECR app image (`spawn app launch --image`).
			map[string]interface{}{
				"Sid":    "PullAppImage",
				"Effect": "Allow",
				"Action": []interface{}{
					"ecr:GetAuthorizationToken",
					"ecr:BatchCheckLayerAvailability",
					"ecr:GetDownloadUrlForLayer",
					"ecr:BatchGetImage",
				},
				"Resource": "*",
			},
			// spored from the release buckets, as the shared role reads it.
			map[string]interface{}{
				"Sid":      "SporedBinaries",
				"Effect":   "Allow",
				"Action":   []interface{}{"s3:GetObject", "s3:GetObjectVersion", "s3:ListBucket", "s3:GetBucketLocation"},
				"Resource": []interface{}{"arn:aws:s3:::spawn-binaries-*", "arn:aws:s3:::spawn-binaries-*/*"},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// sporedTLSKeyDeny is the shared role's refusal of the certificate read.
func sporedTLSKeyDeny() map[string]interface{} {
	return map[string]interface{}{
		"Sid":      "DenyWildcardTLSKey",
		"Effect":   "Deny",
		"Action":   "s3:GetObject",
		"Resource": "arn:aws:s3:::spawn-certs-*/*",
	}
}

func sporedTLSKeyDenyJSON() string {
	b, err := json.Marshal(sporedTLSKeyDeny())
	if err != nil {
		panic(err) // static content; cannot fail
	}
	return string(b)
}

// SetupAppInstanceProfile creates or reuses AppRoleName and its instance
// profile for `spawn app launch`, and returns the profile name: the spored
// baseline and SSM, as every spawn-instance-* role has, plus
// appRolePolicyDoc.
func (c *Client) SetupAppInstanceProfile(ctx context.Context) (string, error) {
	acct, _, err := c.GetCallerIdentityInfo(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve the account for %s: %w", AppRoleName, err)
	}
	doc, err := appRolePolicyDoc(acct)
	if err != nil {
		return "", err
	}
	return c.CreateOrGetInstanceProfile(ctx, IAMRoleConfig{
		RoleName:         AppRoleName,
		InlinePolicyJSON: doc,
		TrustServices:    []string{"ec2"},
		Tags:             map[string]string{"spawn:purpose": "app"},
	})
}
