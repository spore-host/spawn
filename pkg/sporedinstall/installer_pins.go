package sporedinstall

import (
	"regexp"
	"strings"
)

// sha256 pins for the two Amazon installers the Windows bootstrap runs as
// SYSTEM (#1620): the AWS CLI v2 MSI and the SSM agent's setup executable.
// Authenticode (WindowsAmazonSignedFunc, #1598) proves Amazon signed a file;
// a pin also proves it is the one file reviewed here, so a validly signed but
// different build (an older one with a known flaw, say) is refused too.
//
// Written by .github/workflows/installer-pins.yml through scripts/installerpin,
// never by hand: that workflow downloads each installer on a Windows runner,
// requires a Valid Amazon Authenticode signature, and opens a PR with the
// version and sha256. Empty until its first run; while a pair is empty the
// bootstrap installs as before (Authenticode only), and once it is set the
// bootstrap runs nothing whose sha256 differs. Each version/sha256 pair is set
// or empty together (TestInstallerPinsAreWellFormed).
const (
	// The AWS CLI MSI, AWSCLIV2-<version>.msi. The workflow moves it to the
	// release the Linux bootstrap pins (pkg/launcher awsCLIVersion), so the
	// two follow each other a run apart; until it does, Windows keeps
	// installing the MSI pinned here.
	windowsAWSCLIMSIVersion = "2.39.3"
	windowsAWSCLIMSISHA256  = "421292ebfc0fb591f202b0b5644faf875966acbcffa4f543ec13fe709ec009f5"
	// The SSM agent, amazon-ssm-<region>/<version>/windows_amd64/
	// AmazonSSMAgentSetup.exe, the newest aws/amazon-ssm-agent release the
	// workflow could verify.
	windowsSSMAgentVersion = "3.3.5828.0"
	windowsSSMAgentSHA256  = "e216f63ef75822f6bec245d796db4a1da7c0a5985ec5f6881980ec9513075f25"
)

// WindowsInstallerPin is a pinned installer: its version and sha256.
type WindowsInstallerPin struct {
	Version string
	SHA256  string
}

// Set reports whether both halves are present.
func (p WindowsInstallerPin) Set() bool { return p.Version != "" && p.SHA256 != "" }

// WindowsAWSCLIMSIPin is the pinned AWS CLI MSI, or the zero pin.
func WindowsAWSCLIMSIPin() WindowsInstallerPin {
	return WindowsInstallerPin{Version: windowsAWSCLIMSIVersion, SHA256: windowsAWSCLIMSISHA256}
}

// WindowsSSMAgentPin is the pinned SSM agent setup, or the zero pin.
func WindowsSSMAgentPin() WindowsInstallerPin {
	return WindowsInstallerPin{Version: windowsSSMAgentVersion, SHA256: windowsSSMAgentSHA256}
}

// WindowsSHA256FuncName is the PowerShell function WindowsSHA256Func defines.
const WindowsSHA256FuncName = "Assert-SpawnSHA256"

// WindowsSHA256Func is the PowerShell definition of WindowsSHA256FuncName
// <path> <sha256>: it throws unless the file's sha256 is the pinned one.
// Windows PowerShell 5.1 syntax.
const WindowsSHA256Func = `
function Assert-SpawnSHA256([string]$Path, [string]$Want) {
  $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
  if ($got -ne $Want) { throw "spawn: refusing $Path - its sha256 is $got, not the pinned $Want (#1620)" }
}
`

// WindowsInstallerFuncs is every check an installer download passes, for the
// scripts that splice the downloads below in.
const WindowsInstallerFuncs = WindowsAmazonSignedFunc + WindowsSHA256Func

var psSafe = regexp.MustCompile(`^[0-9A-Za-z.-]*$`)

// psLiteral refuses anything that would need quoting in a PowerShell single-
// quoted string. Every value here is a version or a hex digest.
func psLiteral(s string) string {
	if !psSafe.MatchString(s) {
		panic("sporedinstall: unexpected character in installer pin " + s)
	}
	return s
}

// WindowsAWSCLIMSIDownload is PowerShell that downloads the AWS CLI MSI to
// $msi and throws unless it is Amazon-signed and, when pin is set, has the
// pinned sha256. With pin set it fetches exactly that release. Without it,
// fallbackVersion (the Linux pin) is tried first and the unversioned latest
// after it, Authenticode only, as before the pins existed. Indented for a
// try block.
func WindowsAWSCLIMSIDownload(pin WindowsInstallerPin, fallbackVersion string) string {
	return windowsAWSCLIMSIDownload(pin, fallbackVersion, "    ")
}

func windowsAWSCLIMSIDownload(pin WindowsInstallerPin, fallbackVersion, in string) string {
	var b strings.Builder
	switch {
	case pin.Set():
		b.WriteString(in + "# The pinned release, Amazon-signed and with the pinned sha256, or not run at all (#1598, #1620).\n")
		b.WriteString(in + "Invoke-WebRequest -Uri '" + WindowsAWSCLIMSIURL(psLiteral(pin.Version)) + "' -OutFile $msi -UseBasicParsing\n")
		b.WriteString(in + WindowsAmazonSignedFuncName + " $msi\n")
		b.WriteString(in + WindowsSHA256FuncName + " $msi '" + psLiteral(pin.SHA256) + "'\n")
	case strings.TrimSpace(fallbackVersion) != "":
		b.WriteString(in + "# The release the Linux bootstrap pins, else the latest: Amazon-signed\n")
		b.WriteString(in + "# either way, or not run at all (#1598).\n")
		b.WriteString(in + "try { Invoke-WebRequest -Uri '" + WindowsAWSCLIMSIURL(psLiteral(fallbackVersion)) + "' -OutFile $msi -UseBasicParsing }\n")
		b.WriteString(in + "catch { Invoke-WebRequest -Uri '" + WindowsAWSCLIMSIURL("") + "' -OutFile $msi -UseBasicParsing }\n")
		b.WriteString(in + WindowsAmazonSignedFuncName + " $msi\n")
	default:
		b.WriteString(in + "Invoke-WebRequest -Uri '" + WindowsAWSCLIMSIURL("") + "' -OutFile $msi -UseBasicParsing\n")
		b.WriteString(in + "# Amazon-signed or not run at all (#1598).\n")
		b.WriteString(in + WindowsAmazonSignedFuncName + " $msi\n")
	}
	return b.String()
}

// WindowsSSMAgentSetupURLs are the sources of a pinned SSM agent setup: the
// instance's regional amazon-ssm bucket ($region, set by the bootstrap), then
// us-east-1's, both by their regional names, which an S3 endpoint serves
// (#1436).
func WindowsSSMAgentSetupURLs(version string) []string {
	v := psLiteral(version)
	return []string{
		"https://amazon-ssm-${region}.s3.${region}.amazonaws.com/" + v + "/windows_amd64/AmazonSSMAgentSetup.exe",
		"https://amazon-ssm-us-east-1.s3.us-east-1.amazonaws.com/" + v + "/windows_amd64/AmazonSSMAgentSetup.exe",
	}
}

// WindowsSSMAgentDownload is PowerShell that downloads the SSM agent's setup
// to $msi and throws unless it is Amazon-signed and, when pin is set, is the
// pinned version with the pinned sha256, from the first source that passes.
// Without a pin it is the regional latest, Authenticode only. Indented for
// the if block inside a try.
func WindowsSSMAgentDownload(pin WindowsInstallerPin) string {
	if !pin.Set() {
		return "    Invoke-WebRequest -Uri \"https://amazon-ssm-${region}.s3.${region}.amazonaws.com/latest/windows_amd64/AmazonSSMAgentSetup.exe\" -OutFile $msi -UseBasicParsing\n" +
			"    # Amazon-signed or not run at all (#1598).\n" +
			"    " + WindowsAmazonSignedFuncName + " $msi\n"
	}
	var srcs []string
	for _, u := range WindowsSSMAgentSetupURLs(pin.Version) {
		srcs = append(srcs, `"`+u+`"`)
	}
	return "    # The pinned version, Amazon-signed and with the pinned sha256, from the\n" +
		"    # first source that has it, or not run at all (#1598, #1620).\n" +
		"    $ssmOK = $false\n" +
		"    foreach ($u in @(" + strings.Join(srcs, ", ") + ")) {\n" +
		"      try {\n" +
		"        Invoke-WebRequest -Uri $u -OutFile $msi -UseBasicParsing\n" +
		"        " + WindowsAmazonSignedFuncName + " $msi\n" +
		"        " + WindowsSHA256FuncName + " $msi '" + psLiteral(pin.SHA256) + "'\n" +
		"        $ssmOK = $true\n" +
		"        break\n" +
		"      } catch {\n" +
		"        Write-Output \"spawn: SSM agent " + pin.Version + " from $u refused: $_\"\n" +
		"        Remove-Item -Force -ErrorAction SilentlyContinue $msi\n" +
		"      }\n" +
		"    }\n" +
		"    if (-not $ssmOK) { throw \"spawn: no source had a verified SSM agent " + pin.Version + " (#1620)\" }\n"
}
