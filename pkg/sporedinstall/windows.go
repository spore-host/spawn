package sporedinstall

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
)

// The Windows half of the verified spored install (#879, GHSA-q59f-6gc3-qj2p).
//
// The Linux bootstrap has verified spored's .sha256 and detached .sig and failed
// closed since GHSA-3x4q-99gh-fxhx. The Windows bootstrap did neither: it copied
// spored-windows-amd64.exe out of S3 and registered it as a SYSTEM service, and
// continued with a warning when the download failed. The material to verify
// against was published all along — every release writes the .exe's .sha256 and a
// KMS signature beside it.
//
// Target: Windows PowerShell 5.1 on Windows 11+ (.NET Framework 4.8.1). Not
// PowerShell 7: EC2Launch runs <powershell> user-data with the built-in
// powershell.exe, and PowerShell 7 is not preinstalled even on Windows 11, so
// requiring it would mean downloading an unverified installer before verifying
// spored. Two consequences shape the code below:
//
//   - The key is embedded as its X/Y coordinates, not as PEM. .NET Framework has
//     no ImportSubjectPublicKeyInfo (that is .NET Core 3+), but it does have
//     ECDsa.Create(ECParameters), which takes the curve point directly. The
//     coordinates are derived from SigningKeyPEM at generation time, so Linux and
//     Windows cannot verify against different keys.
//   - The release signature is ASN.1 DER (what KMS emits and openssl verifies), but
//     .NET's VerifyHash takes IEEE P1363 r||s. derToP1363 converts.
//
// Everything used also exists in PowerShell 7 on .NET 8, which is what lets the
// generated script be EXECUTED in CI rather than string-matched. Syntax is kept to
// 5.1: no ?:, no ??, no ForEach-Object -Parallel.

// WindowsInstallScript returns the PowerShell that downloads spored.exe with its
// .sha256 and .sig from the first source that verifies, and installs it as a
// service only after verification. On every failure it installs NOTHING and says
// so on the console, which is what GetConsoleOutput shows an operator.
//
// keyPEM is the verification key; pass SigningKeyPEM. Empty means a spawn build
// with no signing key compiled in, and — exactly as the Linux path's
// SPORED_SIG_VERIFY=0 — the checksum is still required but the signature is not.
//
// The returned script expects $aws (path to aws.exe) and $region to be set, and
// sets $sporedInstalled to $true or $false.
func WindowsInstallScript(keyPEM string) (string, error) {
	sigVerify := "$false"
	keyX, keyY := "", ""
	if strings.TrimSpace(keyPEM) != "" {
		x, y, err := p256Coordinates(keyPEM)
		if err != nil {
			return "", fmt.Errorf("spored signing key: %w", err)
		}
		sigVerify, keyX, keyY = "$true", x, y
	}
	return strings.NewReplacer(
		"__SIG_VERIFY__", sigVerify,
		"__KEY_X__", keyX,
		"__KEY_Y__", keyY,
	).Replace(windowsInstallTemplate), nil
}

// p256Coordinates returns the hex X and Y of a PEM-encoded P-256 public key, each
// left-padded to 32 bytes — the fixed width .NET's ECPoint requires.
func p256Coordinates(keyPEM string) (string, string, error) {
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return "", "", fmt.Errorf("not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", "", err
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return "", "", fmt.Errorf("not a P-256 ECDSA key")
	}
	// Bytes is the SEC 1 uncompressed point, 0x04 || X || Y, with each coordinate
	// already fixed at 32 bytes for P-256 — the padding .NET needs (#923: replaces
	// the deprecated PublicKey.X/.Y, whose big.Int bytes had to be left-padded).
	pt, err := ec.Bytes()
	if err != nil {
		return "", "", err
	}
	if len(pt) != 65 || pt[0] != 4 {
		return "", "", fmt.Errorf("unexpected P-256 point encoding (%d bytes)", len(pt))
	}
	return hex.EncodeToString(pt[1:33]), hex.EncodeToString(pt[33:65]), nil
}

// windowsInstallTemplate is the PowerShell itself. Placeholders are replaced by
// WindowsInstallScript; nothing else is templated, so the script cannot be
// broken by a value containing a format verb.
const windowsInstallTemplate = `
$sporeSigVerify = __SIG_VERIFY__
$sporeKeyX = '__KEY_X__'
$sporeKeyY = '__KEY_Y__'

function ConvertFrom-SporeHex([string]$h) {
  $b = New-Object byte[] ($h.Length / 2)
  for ($i = 0; $i -lt $b.Length; $i++) { $b[$i] = [Convert]::ToByte($h.Substring($i * 2, 2), 16) }
  return ,$b
}

# ASN.1 DER ECDSA signature -> IEEE P1363 r||s (32 bytes each for P-256).
# DER is SEQUENCE { INTEGER r, INTEGER s }; an INTEGER carries a leading 0x00
# when its high bit is set, and may be shorter than 32 bytes.
function ConvertFrom-SporeDerSignature([byte[]]$der) {
  if ($der.Length -lt 8 -or $der[0] -ne 0x30) { throw 'signature is not a DER SEQUENCE' }
  $p = 2
  if ($der[1] -band 0x80) { $p = 2 + ($der[1] -band 0x7f) }
  $out = New-Object byte[] 64
  for ($k = 0; $k -lt 2; $k++) {
    if ($der[$p] -ne 0x02) { throw 'signature component is not an INTEGER' }
    $len = [int]$der[$p + 1]
    $start = $p + 2
    while ($len -gt 32 -and $der[$start] -eq 0) { $start++; $len-- }
    if ($len -gt 32) { throw 'signature component longer than 32 bytes' }
    [Array]::Copy($der, $start, $out, ($k * 32) + (32 - $len), $len)
    $p = $start + $len
  }
  return ,$out
}

# Verify one downloaded spored.exe. Throws on any failure; returns nothing on success.
function Test-SporeBinary([string]$exe, [string]$shaFile, [string]$sigFile) {
  $want = (((Get-Content -Raw -Path $shaFile).Trim()) -split '\s+')[0].ToLowerInvariant()
  if ($want.Length -ne 64) { throw "malformed .sha256 ($want)" }
  $sha = [System.Security.Cryptography.SHA256]::Create()
  $fs = [System.IO.File]::OpenRead($exe)
  try { $digest = $sha.ComputeHash($fs) } finally { $fs.Dispose() }
  $got = (($digest | ForEach-Object { $_.ToString('x2') }) -join '')
  if ($got -ne $want) { throw "checksum mismatch (got $got, published $want)" }
  if (-not $sporeSigVerify) { return }
  $der = [Convert]::FromBase64String((Get-Content -Raw -Path $sigFile).Trim())
  $sig = ConvertFrom-SporeDerSignature $der
  $params = New-Object System.Security.Cryptography.ECParameters
  $params.Curve = [System.Security.Cryptography.ECCurve+NamedCurves]::nistP256
  $q = New-Object System.Security.Cryptography.ECPoint
  $q.X = ConvertFrom-SporeHex $sporeKeyX
  $q.Y = ConvertFrom-SporeHex $sporeKeyY
  $params.Q = $q
  $ec = [System.Security.Cryptography.ECDsa]::Create($params)
  try {
    if (-not $ec.VerifyHash($digest, $sig)) { throw 'signature does not verify against the spore.host key' }
  } finally { $ec.Dispose() }
}

# Try each source in order. A source counts only if its binary, .sha256 and (when
# verifying) .sig all download AND verify; a source that fails verification is
# skipped, never installed. Nothing is installed unless one source verifies.
$sporedInstalled = $false
$dir = Join-Path $env:ProgramFiles 'spored'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$exe = Join-Path $dir 'spored.exe'
$tmp = Join-Path $env:TEMP ('spored-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$bucket = "spawn-binaries-$region"
$sources = @(
  @{ u = "s3://$bucket/spawn/spored-windows-amd64.exe"; r = $region },
  @{ u = "s3://$bucket/spored-windows-amd64.exe"; r = $region },
  @{ u = 's3://spawn-binaries-us-east-1/spawn/spored-windows-amd64.exe'; r = 'us-east-1' }
)
foreach ($src in $sources) {
  $cand = Join-Path $tmp 'spored.exe'
  Remove-Item -Force -ErrorAction SilentlyContinue $cand, "$cand.sha256", "$cand.sig"
  & $aws s3 cp $src.u $cand --region $src.r 2>$null
  if ($LASTEXITCODE -ne 0) { continue }
  & $aws s3 cp ($src.u + '.sha256') "$cand.sha256" --region $src.r 2>$null
  if ($LASTEXITCODE -ne 0) { Write-Output "spawn: no .sha256 at $($src.u) - skipping this source"; continue }
  if ($sporeSigVerify) {
    & $aws s3 cp ($src.u + '.sig') "$cand.sig" --region $src.r 2>$null
    if ($LASTEXITCODE -ne 0) { Write-Output "spawn: no .sig at $($src.u) - skipping this source"; continue }
  }
  try {
    Test-SporeBinary $cand "$cand.sha256" "$cand.sig"
  } catch {
    Write-Output "spawn: REFUSING spored from $($src.u): $_"
    continue
  }
  Move-Item -Force $cand $exe
  $sporedInstalled = $true
  Write-Output "spawn: spored verified from $($src.u)"
  break
}
Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $tmp
if (-not $sporedInstalled) {
  Write-Output 'spawn: spored was NOT installed - no source passed checksum and signature verification. This instance has NO TTL, idle-stop or cost-limit enforcement; terminate it with: spawn terminate'
}
`

// WindowsStandaloneScript is scripts/install-spored.ps1: the manual installer for
// an existing Windows instance. Windows has no `spawn upgrade-spored` yet (#234),
// so this is the only manual path there is — and it used to download spored and
// register it as a SYSTEM service with no checksum or signature check.
//
// Generated from the SAME WindowsInstallScript the launch bootstrap uses, rather
// than maintained by hand, so the two cannot drift. The committed file is checked
// against this output by TestInstallSporedPs1IsGenerated; regenerate it with
// `UPDATE_GOLDEN=1 go test ./pkg/sporedinstall -run TestInstallSporedPs1IsGenerated`.
func WindowsStandaloneScript(keyPEM string) (string, error) {
	core, err := WindowsInstallScript(keyPEM)
	if err != nil {
		return "", err
	}
	return windowsStandalonePreamble + core + windowsStandaloneEpilogue, nil
}

const windowsStandalonePreamble = `<#
.SYNOPSIS
  Install spored on an existing Windows EC2 instance, verified.

.DESCRIPTION
  GENERATED by pkg/sporedinstall.WindowsStandaloneScript — do not edit by hand.
  Regenerate: UPDATE_GOLDEN=1 go test ./pkg/sporedinstall -run TestInstallSporedPs1IsGenerated

  Downloads spored.exe with its .sha256 and .sig from the regional spawn-binaries
  bucket (falling back to us-east-1), verifies BOTH against the spore.host key —
  the same check the launch bootstrap performs — and installs it as a service only
  if they pass. Exits 1, installing nothing, if no source verifies.

  Windows PowerShell 5.1 (the built-in powershell.exe) is the target. Run elevated.
#>

# Not 'Stop': under Windows PowerShell 5.1, redirecting a native command's stderr
# with 'Stop' in effect turns any stderr output from aws.exe into a terminating
# error, which would abort the source fallback below on a routine 404.
$ErrorActionPreference = 'Continue'

function Get-Ec2Region {
  try {
    $token = Invoke-RestMethod -Method Put -Uri 'http://169.254.169.254/latest/api/token' -Headers @{ 'X-aws-ec2-metadata-token-ttl-seconds' = '21600' } -TimeoutSec 5
    return Invoke-RestMethod -Uri 'http://169.254.169.254/latest/meta-data/placement/region' -Headers @{ 'X-aws-ec2-metadata-token' = $token } -TimeoutSec 5
  } catch { return $null }
}
$region = Get-Ec2Region
if (-not $region) { Write-Output 'WARNING: could not detect region, using us-east-1'; $region = 'us-east-1' }

$aws = "$env:ProgramFiles\Amazon\AWSCLIV2\aws.exe"
if (-not (Test-Path $aws)) {
  $msi = "$env:TEMP\AWSCLIV2.msi"
  Invoke-WebRequest -Uri 'https://awscli.amazonaws.com/AWSCLIV2.msi' -OutFile $msi -UseBasicParsing
  Start-Process msiexec.exe -ArgumentList @('/i', $msi, '/qn') -Wait
}
`

const windowsStandaloneEpilogue = `
if (-not $sporedInstalled) { exit 1 }
& $exe service install $exe
& $exe service start
Write-Output 'spored installed and started.'
`
