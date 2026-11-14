# AgentFixture Universal PowerShell One-Line Installer (Windows amd64/arm64)
# Usage:
#   irm https://raw.githubusercontent.com/aloneangel45/AgentFixture/main/install.ps1 | iex
#   $env:AGENTFIXTURE_VERSION = "v1.2.0"; irm https://raw.githubusercontent.com/aloneangel45/AgentFixture/main/install.ps1 | iex

$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Write-AFInfo {
    param([string]$Message)
    Write-Host "[AgentFixture] $Message"
}

function Get-AFArchitecture {
    $rawArch = $null
    try {
        $rawArch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
    } catch {
        $rawArch = $null
    }
    if ([string]::IsNullOrWhiteSpace($rawArch)) {
        if (-not [string]::IsNullOrWhiteSpace($env:PROCESSOR_ARCHITEW6432)) {
            $rawArch = $env:PROCESSOR_ARCHITEW6432
        } else {
            $rawArch = $env:PROCESSOR_ARCHITECTURE
        }
    }

    switch -Regex ($rawArch.ToUpperInvariant()) {
        '^(AMD64|X64)$'   { return 'amd64' }
        '^(ARM64|AARCH64)$' { return 'arm64' }
        default {
            throw "Unsupported Windows CPU architecture '$rawArch' (supported: amd64, arm64)."
        }
    }
}

function Get-AFBaseUrl {
    param([string]$Repo, [string]$Version)
    if (-not [string]::IsNullOrWhiteSpace($env:AGENTFIXTURE_BASE_URL)) {
        return $env:AGENTFIXTURE_BASE_URL.TrimEnd('/')
    }
    if ([string]::IsNullOrWhiteSpace($Version) -or $Version -eq 'latest') {
        return "https://github.com/$Repo/releases/latest/download"
    }
    $tag = if ($Version.StartsWith('v')) { $Version } else { "v$Version" }
    return "https://github.com/$Repo/releases/download/$tag"
}

function Get-ExpectedSha256 {
    param([string]$ChecksumsPath, [string]$AssetName)
    foreach ($line in Get-Content -Path $ChecksumsPath) {
        $trimmed = $line.Trim()
        if ([string]::IsNullOrWhiteSpace($trimmed)) {
            continue
        }
        $parts = $trimmed -split '\s+', 2
        if ($parts.Count -lt 2) {
            continue
        }
        $hash = $parts[0].Trim().ToLowerInvariant()
        $file = $parts[1].Trim().TrimStart('*')
        if ($file.StartsWith('./')) {
            $file = $file.Substring(2)
        }
        if ($file -eq $AssetName) {
            return $hash
        }
    }
    return $null
}

$Repo = if (-not [string]::IsNullOrWhiteSpace($env:AGENTFIXTURE_REPO)) { $env:AGENTFIXTURE_REPO } else { 'aloneangel45/AgentFixture' }
$Version = if (-not [string]::IsNullOrWhiteSpace($env:AGENTFIXTURE_VERSION)) { $env:AGENTFIXTURE_VERSION } else { 'latest' }
$Arch = Get-AFArchitecture
$BinaryName = 'agentfixture.exe'
$Asset = "agentfixture-windows-$Arch.zip"
$BaseUrl = Get-AFBaseUrl -Repo $Repo -Version $Version

$InstallDir = if (-not [string]::IsNullOrWhiteSpace($env:AGENTFIXTURE_INSTALL_DIR)) {
    $env:AGENTFIXTURE_INSTALL_DIR
} elseif (-not [string]::IsNullOrWhiteSpace($env:LOCALAPPDATA)) {
    Join-Path $env:LOCALAPPDATA 'Programs\AgentFixture\bin'
} else {
    Join-Path $HOME '.agentfixture\bin'
}

$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("agentfixture-install-" + [System.Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null

try {
    $ArchivePath = Join-Path $TempDir $Asset
    $ChecksumsPath = Join-Path $TempDir 'checksums.txt'
    $ExtractDir = Join-Path $TempDir 'extracted'

    Write-AFInfo "Downloading $Asset (windows/$Arch) from $BaseUrl..."
    Invoke-WebRequest -Uri "$BaseUrl/$Asset" -OutFile $ArchivePath -UseBasicParsing

    Write-AFInfo "Downloading checksums.txt for mandatory SHA-256 verification..."
    Invoke-WebRequest -Uri "$BaseUrl/checksums.txt" -OutFile $ChecksumsPath -UseBasicParsing

    $ExpectedSha = Get-ExpectedSha256 -ChecksumsPath $ChecksumsPath -AssetName $Asset
    if ([string]::IsNullOrWhiteSpace($ExpectedSha)) {
        throw "Asset '$Asset' was not found in checksums.txt. Aborting installation."
    }

    $Sha256Hasher = [System.Security.Cryptography.SHA256]::Create()
    $ArchiveStream = [System.IO.File]::OpenRead($ArchivePath)
    try {
        $ActualSha = [System.BitConverter]::ToString($Sha256Hasher.ComputeHash($ArchiveStream)).Replace('-', '').ToLowerInvariant()
    } finally {
        $ArchiveStream.Dispose()
        $Sha256Hasher.Dispose()
    }
    if ($ActualSha -ne $ExpectedSha) {
        throw "SHA-256 checksum mismatch for $Asset! Expected: $ExpectedSha, Actual: $ActualSha"
    }
    Write-AFInfo "SHA-256 verified: $ActualSha"

    Add-Type -AssemblyName System.IO.Compression.FileSystem
    if (Test-Path -LiteralPath $ExtractDir) {
        Remove-Item -LiteralPath $ExtractDir -Recurse -Force
    }
    [System.IO.Compression.ZipFile]::ExtractToDirectory($ArchivePath, $ExtractDir)
    $ExtractedBin = Join-Path $ExtractDir $BinaryName
    if (-not (Test-Path -LiteralPath $ExtractedBin)) {
        throw "Expected binary '$BinaryName' was not found inside $Asset."
    }

    if (-not (Test-Path -LiteralPath $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    }

    $DestBin = Join-Path $InstallDir $BinaryName
    Copy-Item -LiteralPath $ExtractedBin -Destination $DestBin -Force

    if ($env:AGENTFIXTURE_NO_PATH_UPDATE -ne '1') {
        $UserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $PathEntries = if ([string]::IsNullOrWhiteSpace($UserPath)) { @() } else { $UserPath -split ';' }
        $AlreadyInPath = $false
        foreach ($entry in $PathEntries) {
            if ($entry.TrimEnd('\') -ieq $InstallDir.TrimEnd('\')) {
                $AlreadyInPath = $true
                break
            }
        }
        if (-not $AlreadyInPath) {
            $UpdatedPath = if ([string]::IsNullOrWhiteSpace($UserPath)) { $InstallDir } else { "$UserPath;$InstallDir" }
            [Environment]::SetEnvironmentVariable('Path', $UpdatedPath, 'User')
            Write-AFInfo "Added $InstallDir to User PATH."
        }
        if (($env:Path -split ';') -notcontains $InstallDir) {
            $env:Path = "$InstallDir;$env:Path"
        }
    }

    Write-AFInfo "Installed successfully to $DestBin"
    & $DestBin version
} finally {
    if (Test-Path -LiteralPath $TempDir) {
        Remove-Item -LiteralPath $TempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
