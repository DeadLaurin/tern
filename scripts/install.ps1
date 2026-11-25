<#
.SYNOPSIS
    Install tern from GitHub releases on Windows.

.DESCRIPTION
    Downloads the tern Windows .zip and checksums.txt from GitHub releases,
    verifies the SHA-256 checksum, installs tern.exe into
    %LOCALAPPDATA%\Programs\tern, and adds that directory to the user PATH
    if it is not already present.

.PARAMETER Version
    Release version to install (e.g. "1.2.3" or "v1.2.3").
    Defaults to $env:TERN_VERSION, or the latest release when unset.

.PARAMETER InstallDir
    Install directory. Defaults to $env:TERN_INSTALL_DIR, or
    %LOCALAPPDATA%\Programs\tern.

.EXAMPLE
    irm https://raw.githubusercontent.com/tern-agent/tern/main/scripts/install.ps1 | iex

.EXAMPLE
    .\install.ps1 -Version 1.2.3
#>
[CmdletBinding()]
param(
    [string]$Version = $env:TERN_VERSION,
    [string]$InstallDir = $env:TERN_INSTALL_DIR
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Repo = if ($env:TERN_REPO) { $env:TERN_REPO } else { 'tern-agent/tern' }
$BaseUrl = "https://github.com/$Repo"
$DownloadsUrl = "$BaseUrl/releases/download"

function Write-Info([string]$Message) {
    Write-Host $Message
}

function Write-Fail([string]$Message) {
    Write-Error "error: $Message" -ErrorAction Continue
    exit 1
}

# Windows PowerShell 5.1 defaults to older TLS versions on some systems.
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
}
catch {
    # Ignore: newer runtimes manage this automatically.
}

function Get-RemoteFile([string]$Uri, [string]$Destination) {
    Invoke-WebRequest -Uri $Uri -OutFile $Destination -UseBasicParsing
}

if (-not $Version) {
    Write-Info 'Looking up the latest tern release...'
    try {
        $release = Invoke-RestMethod `
            -Uri "https://api.github.com/repos/$Repo/releases/latest" `
            -Headers @{ 'User-Agent' = 'tern-installer'; 'Accept' = 'application/vnd.github+json' } `
            -UseBasicParsing
        $Version = $release.tag_name
    }
    catch {
        Write-Fail "could not query the latest tern release at $BaseUrl/releases: $($_.Exception.Message)"
    }
}

$Version = $Version.TrimStart('v')
if (-not $Version) {
    Write-Fail 'could not determine the tern version to install'
}
$Tag = "v$Version"

$architecture = $null
try {
    $architecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
}
catch {
    $architecture = $env:PROCESSOR_ARCHITECTURE
}

switch ($architecture) {
    'X64' { $goarch = 'amd64' }
    'Amd64' { $goarch = 'amd64' }
    'AMD64' { $goarch = 'amd64' }
    'Arm64' { $goarch = 'arm64' }
    'ARM64' { $goarch = 'arm64' }
    default {
        Write-Fail "unsupported architecture: $architecture (supported: amd64, arm64)"
    }
}

if (-not $InstallDir) {
    $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\tern'
}

$asset = "tern_${Version}_windows_${goarch}.zip"
$checksumsAsset = 'checksums.txt'
$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("tern-install-" + [System.Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tempDir -Force | Out-Null

try {
    $zipPath = Join-Path $tempDir $asset
    $checksumsPath = Join-Path $tempDir $checksumsAsset
    $extractDir = Join-Path $tempDir 'extract'

    Write-Info "Installing tern $Version (windows/$goarch)"

    try {
        Get-RemoteFile -Uri "$DownloadsUrl/$Tag/$asset" -Destination $zipPath
        Get-RemoteFile -Uri "$DownloadsUrl/$Tag/$checksumsAsset" -Destination $checksumsPath
    }
    catch {
        Write-Fail "failed to download release assets for ${Tag}: $($_.Exception.Message)"
    }

    $expectedHash = $null
    foreach ($line in Get-Content -Path $checksumsPath) {
        if ($line -match '^([0-9a-fA-F]{64})\s+\*?(.+?)\s*$') {
            if ($Matches[2] -eq $asset) {
                $expectedHash = $Matches[1].ToLowerInvariant()
                break
            }
        }
    }

    if (-not $expectedHash) {
        Write-Fail "no checksum found for $asset in checksums.txt"
    }

    $actualHash = (Get-FileHash -Path $zipPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualHash -ne $expectedHash) {
        Write-Fail "checksum verification failed for ${asset}`n  expected: $expectedHash`n  actual:   $actualHash"
    }

    Expand-Archive -Path $zipPath -DestinationPath $extractDir -Force

    $binary = Get-ChildItem -Path $extractDir -Filter 'tern.exe' -File -Recurse |
        Select-Object -First 1
    if (-not $binary) {
        Write-Fail "tern.exe not found in $asset"
    }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $target = Join-Path $InstallDir 'tern.exe'
    Copy-Item -Path $binary.FullName -Destination $target -Force

    Write-Info "Installed tern to $target"

    $normalizedInstallDir = $InstallDir.TrimEnd('\')
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $userPath) { $userPath = '' }

    $pathEntries = @($userPath -split ';' | Where-Object { $_ -ne '' })
    $alreadyOnUserPath = $false
    foreach ($entry in $pathEntries) {
        if ($entry.TrimEnd('\') -ieq $normalizedInstallDir) {
            $alreadyOnUserPath = $true
            break
        }
    }

    if (-not $alreadyOnUserPath) {
        $newUserPath = (@($pathEntries) + $normalizedInstallDir) -join ';'
        [Environment]::SetEnvironmentVariable('Path', $newUserPath, 'User')
        Write-Info "Added $normalizedInstallDir to the user PATH."
    }

    $sessionEntries = @(($env:Path -split ';') | Where-Object { $_ -ne '' })
    $onSessionPath = $false
    foreach ($entry in $sessionEntries) {
        if ($entry.TrimEnd('\') -ieq $normalizedInstallDir) {
            $onSessionPath = $true
            break
        }
    }

    if (-not $onSessionPath) {
        $env:Path = "$env:Path;$normalizedInstallDir"
    }

    if (-not $alreadyOnUserPath -or -not $onSessionPath) {
        Write-Info ''
        Write-Info 'Restart your terminal (or open a new PowerShell window) to use tern.'
        Write-Info "To use it in this session, run: `$env:Path += `";$normalizedInstallDir`""
    }
}
finally {
    Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
}
