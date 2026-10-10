# ── Anvil Installer (Windows) ────────────────────────────────
# Usage: irm https://raw.githubusercontent.com/anvil-cloud/anvil/master/install.ps1 | iex

$ErrorActionPreference = "Stop"

$Repo = "anvil-cloud/anvil"
$InstallDir = if ($env:ANVIL_INSTALL_DIR) { $env:ANVIL_INSTALL_DIR } else { "$env:LOCALAPPDATA\anvil\bin" }
$BinaryName = "anvil.exe"
$ProviderName = "pulumi-resource-anvil.exe"

# ── Detect architecture ──────────────────────────────────────

function Get-Arch {
    switch ($env:PROCESSOR_ARCHITECTURE) {
        "AMD64" { return "amd64" }
        "ARM64" { return "arm64" }
        default {
            Write-Error "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE"
            exit 1
        }
    }
}

# ── Fetch latest version ─────────────────────────────────────

function Get-LatestVersion {
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest"
    return $release.tag_name
}

# ── Main ─────────────────────────────────────────────────────

$Arch = Get-Arch
$Version = if ($env:ANVIL_VERSION) { $env:ANVIL_VERSION } else { Get-LatestVersion }
$VersionNum = $Version.TrimStart("v")
$Archive = "anvil_${VersionNum}_windows_${Arch}.zip"
$Checksums = "checksums.txt"
$DownloadUrl = "https://github.com/$Repo/releases/download/$Version"

$TmpDir = New-Item -ItemType Directory -Path (Join-Path $env:TEMP "anvil-install-$(Get-Random)")

try {
    Write-Host "Installing Anvil $Version (windows/$Arch)..."

    # Download archive and checksums
    Write-Host "  Downloading $Archive..."
    Invoke-WebRequest -Uri "$DownloadUrl/$Archive" -OutFile (Join-Path $TmpDir $Archive)

    Write-Host "  Downloading checksums..."
    Invoke-WebRequest -Uri "$DownloadUrl/$Checksums" -OutFile (Join-Path $TmpDir $Checksums)

    # Verify checksum
    Write-Host "  Verifying checksum..."
    $Expected = (Get-Content (Join-Path $TmpDir $Checksums) | Where-Object { $_ -match $Archive }) -replace "\s+.*", ""
    $Actual = (Get-FileHash -Algorithm SHA256 (Join-Path $TmpDir $Archive)).Hash.ToLower()

    if ($Expected -ne $Actual) {
        Write-Error "Checksum mismatch`n  Expected: $Expected`n  Got:      $Actual"
        exit 1
    }

    # Extract
    Write-Host "  Extracting..."
    Expand-Archive -Path (Join-Path $TmpDir $Archive) -DestinationPath $TmpDir -Force

    # Install
    if (-not (Test-Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    }

    Move-Item -Path (Join-Path $TmpDir $BinaryName) -Destination (Join-Path $InstallDir $BinaryName) -Force
    Move-Item -Path (Join-Path $TmpDir $ProviderName) -Destination (Join-Path $InstallDir $ProviderName) -Force

    # Add to PATH if not already there
    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($UserPath -notlike "*$InstallDir*") {
        [Environment]::SetEnvironmentVariable("Path", "$UserPath;$InstallDir", "User")
        Write-Host ""
        Write-Host "  Added $InstallDir to your PATH."
        Write-Host "  Restart your terminal for it to take effect."
    }

    Write-Host ""
    Write-Host "  ✔ Anvil $Version installed to $InstallDir"
    Write-Host ""
    Write-Host "  Run 'anvil --help' to get started."
}
finally {
    Remove-Item -Recurse -Force $TmpDir -ErrorAction SilentlyContinue
}