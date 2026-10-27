package aws

// SporedBaselinePolicyDocument is the spored self-management policy
// (spored-baseline-policy) that CreateOrGetInstanceProfile attaches to every
// spawn-instance-* role, whatever else the role carries. The account
// placeholder is left unbound: the policy is written per account at attach
// time (bindPolicyAccount).
//
// Exported for the IAM coverage gate (internal/iamcoverage, #254), which holds
// it to the AWS calls spored's code makes. It is the same document the role
// receives, not a copy.
func SporedBaselinePolicyDocument() string {
	return (&Client{}).sporedBaselinePolicyDoc()
}

// SporedInstanceRolePolicyDocument is the inline policy of the shared
// spored-instance-role, which SetupSporedIAMRole attaches and every launch with
// no IAM configuration (and every batch-queue launch without --iam-role) runs
// under. Exported for the IAM coverage gate, as SporedBaselinePolicyDocument.
func SporedInstanceRolePolicyDocument() string {
	return sporedDCVRolePolicy
}
