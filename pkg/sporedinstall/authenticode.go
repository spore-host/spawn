package sporedinstall

import "strings"

// Amazon-signed Windows installers (#1598).
//
// The Windows bootstrap installs the AWS CLI MSI and, where an AMI lacks it,
// the SSM agent's setup executable, both as SYSTEM. They were run as
// downloaded, with no check at all, while every artifact the Linux bootstrap
// installs is pinned and verified. Both are Authenticode-signed by Amazon, so
// before either runs, WindowsAmazonSignedFunc requires a Valid signature whose
// signer is an Amazon organisation, and refuses the file otherwise: the
// installer's try/catch then reports the refusal and nothing runs.

// WindowsAmazonSignedFuncName is the PowerShell function
// WindowsAmazonSignedFunc defines.
const WindowsAmazonSignedFuncName = "Assert-SpawnAmazonSigned"

// WindowsAmazonSignedFunc is the PowerShell definition of
// WindowsAmazonSignedFuncName <path>: it throws unless path carries a Valid
// Authenticode signature (the chain verifies to a trusted root, the file is
// unmodified) from a signer whose subject's organisation is Amazon's
// ("Amazon.com Services LLC", "Amazon Web Services, Inc."). Windows
// PowerShell 5.1 syntax.
const WindowsAmazonSignedFunc = `
function Assert-SpawnAmazonSigned([string]$Path) {
  $s = Get-AuthenticodeSignature -FilePath $Path
  if ($s.Status -ne 'Valid') { throw "spawn: refusing $Path - its Authenticode signature is $($s.Status), not Valid (#1598)" }
  $subject = $s.SignerCertificate.Subject
  if ($subject -notmatch '(^|,\s*)O="?Amazon') { throw "spawn: refusing $Path - signed by '$subject', not Amazon (#1598)" }
}
`

// WindowsAWSCLIMSIURL is the AWS CLI v2 MSI of version, or the unversioned
// latest when version is empty.
func WindowsAWSCLIMSIURL(version string) string {
	if strings.TrimSpace(version) == "" {
		return "https://awscli.amazonaws.com/AWSCLIV2.msi"
	}
	return "https://awscli.amazonaws.com/AWSCLIV2-" + version + ".msi"
}
