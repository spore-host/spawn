<#
.SYNOPSIS
  Install the spored agent on a Windows EC2 instance (the Windows counterpart of
  install-spored.sh). Downloads spored.exe from the regional spawn-binaries S3
  bucket — exactly the same buckets/paths Linux uses — then installs and starts
  it as a Windows Service.

.NOTES
  Run as Administrator (LocalSystem during EC2Launch user-data satisfies this).
  Set $env:PROJECT to use a sister project's bucket prefix (default: spawn).
#>

$ErrorActionPreference = 'Stop'

$Project = if ($env:PROJECT) { $env:PROJECT } else { 'spawn' }
$Binary  = 'spored-windows-amd64.exe'
$InstallDir = Join-Path $env:ProgramFiles 'spored'
$ExePath = Join-Path $InstallDir 'spored.exe'

Write-Output "=== Installing spored (project: $Project) ==="
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

# Detect region via IMDSv2 (token), falling back to IMDSv1, then us-east-1.
function Get-Ec2Region {
    try {
        $token = Invoke-RestMethod -Method Put -Uri 'http://169.254.169.254/latest/api/token' `
            -Headers @{ 'X-aws-ec2-metadata-token-ttl-seconds' = '21600' } -TimeoutSec 5
        return Invoke-RestMethod -Uri 'http://169.254.169.254/latest/meta-data/placement/region' `
            -Headers @{ 'X-aws-ec2-metadata-token' = $token } -TimeoutSec 5
    } catch {
        try {
            return Invoke-RestMethod -Uri 'http://169.254.169.254/latest/meta-data/placement/region' -TimeoutSec 5
        } catch {
            return $null
        }
    }
}

$Region = Get-Ec2Region
if (-not $Region) {
    Write-Output "WARNING: could not detect region, using us-east-1"
    $Region = 'us-east-1'
}
Write-Output "Region: $Region"

$Bucket = "spawn-binaries-$Region"

# Download spored.exe: regional project-prefixed → regional root → us-east-1
# project-prefixed → us-east-1 root. Mirrors install-spored.sh's fallback chain.
$sources = @(
    @{ Uri = "s3://$Bucket/$Project/$Binary";                 Region = $Region },
    @{ Uri = "s3://$Bucket/$Binary";                          Region = $Region },
    @{ Uri = "s3://spawn-binaries-us-east-1/$Project/$Binary"; Region = 'us-east-1' },
    @{ Uri = "s3://spawn-binaries-us-east-1/$Binary";          Region = 'us-east-1' }
)

$downloaded = $false
foreach ($s in $sources) {
    Write-Output "Trying $($s.Uri) ..."
    & aws s3 cp $s.Uri $ExePath --region $s.Region 2>$null
    if ($LASTEXITCODE -eq 0 -and (Test-Path $ExePath)) {
        Write-Output "Downloaded from $($s.Uri)"
        $downloaded = $true
        break
    }
}
if (-not $downloaded) {
    Write-Error "Failed to download spored from any S3 location"
    exit 1
}

# Verify the binary runs.
& $ExePath version
if ($LASTEXITCODE -ne 0) {
    Write-Error "spored binary verification failed"
    exit 1
}

# Install + start the Windows service (spored's own subcommand sets auto-start
# and on-failure restart, matching the systemd unit).
& $ExePath service uninstall 2>$null   # idempotent: remove any prior install
& $ExePath service install $ExePath
if ($LASTEXITCODE -ne 0) {
    Write-Error "spored service install failed"
    exit 1
}
& $ExePath service start
Start-Sleep -Seconds 2

$svc = Get-Service -Name spored -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -eq 'Running') {
    Write-Output "spored service is running"
} else {
    Write-Output "WARNING: spored service is not running (status: $($svc.Status))"
}

Write-Output ""
Write-Output "=== Installation complete ==="
Write-Output "Logs: $env:PROGRAMDATA\spored\spored.log"
Write-Output "Service: Get-Service spored"
