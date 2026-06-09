<#
.SYNOPSIS
  Guest provisioning for the Windows custom AMI build (#83). Packer runs this
  over WinRM after the unattended install. It installs the AWS guest components
  required for `ec2 import-image` to produce a bootable, manageable AMI, then
  syspreps the image (generalize + shutdown).

  Without EC2Launch v2 + the AWS NVMe/ENA drivers + the SSM agent, an imported
  Windows instance fails to boot, has no network, or can't be managed.

.NOTES
  Idempotent where practical. Sysprep is the LAST step — it shuts the VM down.
#>

$ErrorActionPreference = 'Stop'
Write-Output '=== spore.host Windows AMI provisioning ==='

# --- 1. AWS NVMe + ENA + PV drivers -----------------------------------------
# EC2 root volumes are NVMe and networking is ENA; an imported image must carry
# these drivers or it won't boot/network. AWS ships them as a single bundle.
Write-Output 'Installing AWS EC2 drivers (NVMe/ENA/PV)...'
$drv = "$env:TEMP\AWSEC2Drivers.zip"
Invoke-WebRequest -Uri 'https://s3.amazonaws.com/ec2-windows-drivers-downloads/AWSPVDriverSetup.zip' -OutFile $drv -UseBasicParsing
Expand-Archive -Path $drv -DestinationPath "$env:TEMP\awspv" -Force
# AWSPVDriverSetup.msi installs the PV drivers; NVMe/ENA come via EC2Launch's
# driver management on first boot, but install the PV bundle now for safety.
Start-Process msiexec.exe -ArgumentList @('/i', "$env:TEMP\awspv\AWSPVDriverSetup.msi", '/qn') -Wait

# --- 2. EC2Launch v2 ---------------------------------------------------------
# The boot-time agent that handles instance init: admin password, drive
# initialization, running user-data (our spored install). Required for import.
Write-Output 'Installing EC2Launch v2...'
$el = "$env:TEMP\EC2Launch.msi"
Invoke-WebRequest -Uri 'https://s3.amazonaws.com/amazon-ec2launch-v2/windows/amd64/latest/AmazonEC2Launch.msi' -OutFile $el -UseBasicParsing
Start-Process msiexec.exe -ArgumentList @('/i', $el, '/qn') -Wait

# --- 3. SSM agent ------------------------------------------------------------
# spawn connect on Windows uses SSM (Session Manager + RunCommand). The stock
# Windows Server AMI bundles it; a custom image must install it explicitly.
Write-Output 'Installing SSM agent...'
$ssm = "$env:TEMP\SSMAgent.exe"
Invoke-WebRequest -Uri 'https://s3.amazonaws.com/ec2-downloads-windows/SSMAgent/latest/windows_amd64/AmazonSSMAgentSetup.exe' -OutFile $ssm -UseBasicParsing
Start-Process $ssm -ArgumentList @('/quiet') -Wait

# --- 4. OpenSSH server -------------------------------------------------------
# Enables key-based SSH-over-SSM (spawn connect installs the spawn pubkey at
# launch into administrators_authorized_keys). Optional but matches #55/#77.
Write-Output 'Enabling OpenSSH Server...'
Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0 -ErrorAction SilentlyContinue
Set-Service -Name sshd -StartupType Automatic

# NOTE: we deliberately do NOT bake spored into the AMI. spawn installs spored
# from S3 at launch (install-spored.ps1 / buildWindowsUserData, #77), keeping
# this image generic and always picking up the latest spored.

# --- 5. Sysprep: generalize + shutdown (LAST STEP) ---------------------------
# import-image requires a generalized image. EC2Launch ships a sysprep profile;
# use it so first boot on EC2 runs EC2Launch correctly.
Write-Output 'Sysprep (generalize + shutdown)...'
$ec2launch = "$env:ProgramFiles\Amazon\EC2Launch\EC2Launch.exe"
if (Test-Path $ec2launch) {
  & $ec2launch sysprep --shutdown
} else {
  & "$env:WINDIR\System32\Sysprep\sysprep.exe" /generalize /oobe /shutdown /quiet
}
