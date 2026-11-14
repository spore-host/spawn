<#
.SYNOPSIS
  Verify the Windows installers the bootstrap runs as SYSTEM and print their
  pins (#1620): the AWS CLI MSI and the SSM agent's setup.

.DESCRIPTION
  Run weekly on windows-latest by .github/workflows/installer-pins.yml; its PR
  job writes the pins with scripts/installerpin. Run from the checkout root:

    pwsh scripts/installer-pins-windows.ps1           only what moved
    pwsh scripts/installer-pins-windows.ps1 -Force    re-verify the pinned ones too

  AWS CLI MSI: AWSCLIV2-<v>.msi for the release the Linux bootstrap pins
  (pkg/launcher awsCLIVersion), so the Windows MSI follows the Linux zips.
  SSM agent: the newest x.y.z.w tag of github.com/aws/amazon-ssm-agent, from
  amazon-ssm-us-east-1 (the bucket family the bootstrap downloads from), and
  cross-checked against the ec2-downloads-windows copy AWS's install guide
  links: both must be the same bytes. A version not published yet is
  reported and skipped.

  Every file is trusted only once Get-AuthenticodeSignature says Valid with an
  Amazon signer, by the same function the bootstrap runs
  (pkg/sporedinstall.WindowsAmazonSignedFunc, copied below verbatim and held
  to it by TestInstallerPinScriptsAgreeWithTheBootstrap). Writes `changed`,
  `msi_version`, `msi_sha256`, `ssm_version` and `ssm_sha256` to
  $env:GITHUB_OUTPUT when set. Downloads into a temp directory and removes it.
  Calls no AWS API.
#>
param([switch]$Force)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3
Set-Location (Split-Path -Parent $PSScriptRoot)

function Out-GitHub([string]$Name, [string]$Value) {
  if ($env:GITHUB_OUTPUT) { "$Name=$Value" | Out-File -FilePath $env:GITHUB_OUTPUT -Append -Encoding utf8 }
}
Out-GitHub changed false

# BEGIN pkg/sporedinstall.WindowsAmazonSignedFunc
function Assert-SpawnAmazonSigned([string]$Path) {
  $s = Get-AuthenticodeSignature -FilePath $Path
  if ($s.Status -ne 'Valid') { throw "spawn: refusing $Path - its Authenticode signature is $($s.Status), not Valid (#1598)" }
  $subject = $s.SignerCertificate.Subject
  if ($subject -notmatch '(^|,\s*)O="?Amazon') { throw "spawn: refusing $Path - signed by '$subject', not Amazon (#1598)" }
}
# END pkg/sporedinstall.WindowsAmazonSignedFunc

# Fetch url to path. $true when fetched, $false when it is not published
# (403/404, S3's answers for a missing key); anything else throws.
function Get-Installer([string]$Url, [string]$Path) {
  try {
    Invoke-WebRequest -Uri $Url -OutFile $Path -UseBasicParsing -MaximumRetryCount 3 -RetryIntervalSec 5
    return $true
  } catch {
    $code = $null
    if ($_.Exception.PSObject.Properties['Response'] -and $_.Exception.Response) { $code = [int]$_.Exception.Response.StatusCode }
    if ($code -eq 403 -or $code -eq 404) { return $false }
    throw
  }
}

function Get-VerifiedSHA256([string]$Path) {
  Assert-SpawnAmazonSigned $Path
  $s = Get-AuthenticodeSignature -FilePath $Path
  $sum = (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
  Write-Output "$(Split-Path -Leaf $Path): Authenticode $($s.Status), signer '$($s.SignerCertificate.Subject)', sha256 $sum" | Out-Host
  return $sum
}

$show = @{}
$lines = & go -C scripts run ./installerpin show
if ($LASTEXITCODE -ne 0) { throw "installerpin show failed" }
foreach ($l in $lines) {
  $k, $v = $l -split '=', 2
  $show[$k] = $v
}
foreach ($k in 'awsCLIVersion', 'windowsAWSCLIMSIVersion', 'windowsSSMAgentVersion') {
  if (-not $show.ContainsKey($k)) { throw "installerpin show printed no $k" }
}

$work = Join-Path ([System.IO.Path]::GetTempPath()) ("installer-pins-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $work | Out-Null
try {
  # 1. The AWS CLI MSI of the Linux pin.
  $cli = $show['awsCLIVersion']
  if ($cli -notmatch '^2\.\d+\.\d+$') { throw "awsCLIVersion '$cli' is not an AWS CLI v2 release" }
  if ($Force -or $cli -ne $show['windowsAWSCLIMSIVersion']) {
    $msi = Join-Path $work "AWSCLIV2-$cli.msi"
    if (Get-Installer "https://awscli.amazonaws.com/AWSCLIV2-$cli.msi" $msi) {
      $sum = Get-VerifiedSHA256 $msi
      Out-GitHub msi_version $cli
      Out-GitHub msi_sha256 $sum
      Out-GitHub changed true
    } else {
      Write-Output "::warning::AWSCLIV2-$cli.msi is not published yet; the next run will retry"
    }
  } else {
    Write-Output "AWS CLI MSI $cli is pinned already"
  }

  # 2. The newest SSM agent.
  $tags = & git ls-remote --tags --refs https://github.com/aws/amazon-ssm-agent
  if ($LASTEXITCODE -ne 0) { throw "git ls-remote github.com/aws/amazon-ssm-agent failed" }
  $versions = @($tags | ForEach-Object { ($_ -split "`t")[1] -replace '^refs/tags/', '' } | Where-Object { $_ -match '^\d+\.\d+\.\d+\.\d+$' } | Sort-Object { [version]$_ })
  if ($versions.Count -eq 0) { throw "found no x.y.z.w tag in github.com/aws/amazon-ssm-agent: the version source has broken" }
  $ssm = $versions[-1]
  $pinned = $show['windowsSSMAgentVersion']
  if ($pinned -and -not $Force -and [version]$pinned -ge [version]$ssm) {
    Write-Output "SSM agent $pinned is pinned and current (newest tag: $ssm)"
  } else {
    $primary = Join-Path $work "us-east-1-AmazonSSMAgentSetup.exe"
    $guide = Join-Path $work "ec2-downloads-windows-AmazonSSMAgentSetup.exe"
    if (-not (Get-Installer "https://amazon-ssm-us-east-1.s3.us-east-1.amazonaws.com/$ssm/windows_amd64/AmazonSSMAgentSetup.exe" $primary)) {
      Write-Output "::warning::SSM agent $ssm is not published yet; the next run will retry"
    } else {
      $sum = Get-VerifiedSHA256 $primary
      # The copy AWS's install guide links (ec2-downloads-windows, us-east-1)
      # must be the same bytes, or something is wrong at the origin.
      if (Get-Installer "https://ec2-downloads-windows.s3.us-east-1.amazonaws.com/SSMAgent/$ssm/windows_amd64/AmazonSSMAgentSetup.exe" $guide) {
        $gsum = Get-VerifiedSHA256 $guide
        if ($gsum -ne $sum) { throw "SSM agent $ssm differs between amazon-ssm-us-east-1 ($sum) and ec2-downloads-windows ($gsum)" }
      } else {
        Write-Output "::warning::SSM agent $ssm has no ec2-downloads-windows copy to cross-check"
      }
      Out-GitHub ssm_version $ssm
      Out-GitHub ssm_sha256 $sum
      Out-GitHub changed true
    }
  }
} finally {
  Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $work
}
