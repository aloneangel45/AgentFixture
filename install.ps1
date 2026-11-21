# AgentFixture Universal PowerShell One-Line Installer (Windows amd64/arm64)
# Usage:
#   irm https://raw.githubusercontent.com/aloneangel45/AgentFixture/main/install.ps1 | iex
#   $env:AGENTFIXTURE_VERSION = "v1.3.0"; irm https://raw.githubusercontent.com/aloneangel45/AgentFixture/main/install.ps1 | iex

function Install-AgentFixture {
    $ErrorActionPreference = 'Stop'

    # Harden PSModulePath before invoking any commands: unconditionally prepend $PSHome\Modules
    # and strip cross-edition PowerShell 7 module paths when running inside Windows PowerShell 5.1.
    if (-not [string]::IsNullOrWhiteSpace($PSHome)) {
        $psHomeModules = [System.IO.Path]::Combine($PSHome, 'Modules')
        $sep = [System.IO.Path]::PathSeparator
        $isDesktopPS = ($PSVersionTable.PSEdition -ne 'Core') -and ($PSVersionTable.PSVersion.Major -lt 6)
        $cleanModules = New-Object 'System.Collections.Generic.List[string]'
        $cleanModules.Add($psHomeModules)

        if (-not [string]::IsNullOrWhiteSpace($env:PSModulePath)) {
            foreach ($rawEntry in $env:PSModulePath.Split($sep)) {
                $trimmedEntry = $rawEntry.Trim()
                if ([string]::IsNullOrWhiteSpace($trimmedEntry)) {
                    continue
                }
                if ($isDesktopPS -and ($trimmedEntry -match '(?i)(^|[\\/])PowerShell[\\/]7([\\/]|$)')) {
                    continue
                }
                if ([string]::Equals($trimmedEntry.TrimEnd('\', '/'), $psHomeModules.TrimEnd('\', '/'), [System.StringComparison]::OrdinalIgnoreCase)) {
                    continue
                }
                $cleanModules.Add($trimmedEntry)
            }
        }
        $env:PSModulePath = [string]::Join([string]$sep, $cleanModules.ToArray())
    }

    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    } catch {
        # Ignore on platforms where SecurityProtocol setter is restricted
    }

    function Write-AFInfo {
        param([string]$Message)
        [Console]::WriteLine("[AgentFixture] $Message")
    }

    function Invoke-AFDownload {
        param([string]$Uri, [string]$OutFile)
        $wc = New-Object System.Net.WebClient
        try {
            $wc.Headers.Add('User-Agent', 'AgentFixture-Installer')
            $wc.DownloadFile($Uri, $OutFile)
        } finally {
            $wc.Dispose()
        }
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
            '^(AMD64|X64)$'     { return 'amd64' }
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
        foreach ($line in [System.IO.File]::ReadAllLines($ChecksumsPath)) {
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
        [System.IO.Path]::Combine($env:LOCALAPPDATA, 'Programs\AgentFixture\bin')
    } else {
        $userHome = [Environment]::GetFolderPath('UserProfile')
        if ([string]::IsNullOrWhiteSpace($userHome)) {
            $userHome = $HOME
        }
        [System.IO.Path]::Combine($userHome, '.agentfixture\bin')
    }

    $TempDir = [System.IO.Path]::Combine([System.IO.Path]::GetTempPath(), ("agentfixture-install-" + [System.Guid]::NewGuid().ToString('N')))
    $null = [System.IO.Directory]::CreateDirectory($TempDir)

    try {
        $ArchivePath = [System.IO.Path]::Combine($TempDir, $Asset)
        $ChecksumsPath = [System.IO.Path]::Combine($TempDir, 'checksums.txt')
        $ExtractDir = [System.IO.Path]::Combine($TempDir, 'extracted')

        Write-AFInfo "Downloading $Asset (windows/$Arch) from $BaseUrl..."
        Invoke-AFDownload -Uri "$BaseUrl/$Asset" -OutFile $ArchivePath

        Write-AFInfo "Downloading checksums.txt for mandatory SHA-256 verification..."
        Invoke-AFDownload -Uri "$BaseUrl/checksums.txt" -OutFile $ChecksumsPath

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

        if (-not ('System.IO.Compression.ZipFile' -as [type])) {
            $null = [System.Reflection.Assembly]::LoadWithPartialName('System.IO.Compression.FileSystem')
        }
        if (-not ('System.IO.Compression.ZipFile' -as [type])) {
            throw "Unable to load System.IO.Compression.FileSystem assembly for zip extraction."
        }
        if ([System.IO.Directory]::Exists($ExtractDir)) {
            [System.IO.Directory]::Delete($ExtractDir, $true)
        }
        [System.IO.Compression.ZipFile]::ExtractToDirectory($ArchivePath, $ExtractDir)

        $ExtractedBin = [System.IO.Path]::Combine($ExtractDir, $BinaryName)
        if (-not [System.IO.File]::Exists($ExtractedBin)) {
            throw "Expected binary '$BinaryName' was not found inside $Asset."
        }

        if (-not [System.IO.Directory]::Exists($InstallDir)) {
            $null = [System.IO.Directory]::CreateDirectory($InstallDir)
        }

        $DestBin = [System.IO.Path]::Combine($InstallDir, $BinaryName)
        if ([System.IO.File]::Exists($DestBin)) {
            $existingAttrs = [System.IO.File]::GetAttributes($DestBin)
            if (($existingAttrs -band [System.IO.FileAttributes]::ReadOnly) -ne 0) {
                [System.IO.File]::SetAttributes($DestBin, ($existingAttrs -band (-bnot [System.IO.FileAttributes]::ReadOnly)))
            }
        }
        [System.IO.File]::Copy($ExtractedBin, $DestBin, $true)

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
            $InProcessPath = $false
            foreach ($procEntry in ($env:Path -split ';')) {
                if ($procEntry.TrimEnd('\') -ieq $InstallDir.TrimEnd('\')) {
                    $InProcessPath = $true
                    break
                }
            }
            if (-not $InProcessPath) {
                $env:Path = "$InstallDir;$env:Path"
            }
        }

        Write-AFInfo "Installed successfully to $DestBin"
        & $DestBin version
        return
    } finally {
        if ([System.IO.Directory]::Exists($TempDir)) {
            try {
                [System.IO.Directory]::Delete($TempDir, $true)
            } catch {
                # Ignore transient cleanup locks on Windows
            }
        }
    }
}

Install-AgentFixture
