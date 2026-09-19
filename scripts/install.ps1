param(
    [string]$Version = $env:REPO_SYNC_VERSION,
    [string]$InstallDir = $env:REPO_SYNC_INSTALL_DIR
)

$ErrorActionPreference = "Stop"

$repository = "JorgeMuehlebach/repo-sync"
if (-not $Version) {
    $release = Invoke-RestMethod "https://api.github.com/repos/$repository/releases/latest"
    $Version = $release.tag_name
}
$Version = $Version.TrimStart("v")
if (-not $Version) { throw "Could not determine the Repo Sync version." }
$architecture = switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) {
    "X64" { "amd64" }
    "Arm64" { "arm64" }
    default { throw "Unsupported Windows architecture: $($_)." }
}
if (-not $InstallDir) { $InstallDir = Join-Path $env:LOCALAPPDATA "Programs\repo-sync" }
$archive = "repo-sync_${Version}_windows_${architecture}.zip"
$baseUrl = "https://github.com/$repository/releases/download/v$Version"
$temporaryDir = Join-Path ([System.IO.Path]::GetTempPath()) ("repo-sync-" + [System.Guid]::NewGuid())
$installedBinary = Join-Path $InstallDir "repo-sync.exe"
$candidate = "$installedBinary.new"
$backup = "$installedBinary.previous"

function Test-LegacyLockPresent([string]$ConfigDir) {
    if ((Test-Path -LiteralPath (Join-Path $ConfigDir "config.yaml.lock")) -or
        (Test-Path -LiteralPath (Join-Path $ConfigDir "state.json.lock"))) {
        return $true
    }
    $lockDir = Join-Path $ConfigDir "locks"
    if (Test-Path -LiteralPath $lockDir -PathType Container) {
        return @((Get-ChildItem -LiteralPath $lockDir -Filter "*.lock" -File -ErrorAction Stop)).Count -gt 0
    }
    return $false
}

function Wait-LegacyLocksReleased([string]$ConfigDir) {
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    while (Test-LegacyLockPresent $ConfigDir) {
        if ([DateTime]::UtcNow -ge $deadline) {
            throw "Repo Sync v0.1 still owns, or left behind, a legacy lock below $ConfigDir. The upgrade was not installed. Stop every old repo-sync command and retry; do not delete a live lock."
        }
        Start-Sleep -Seconds 1
    }
}

function Stop-ExistingRepoSync([string]$InstalledBinary) {
    if (-not (Test-Path -LiteralPath $InstalledBinary)) {
        return $false
    }
    if (-not (Test-Path -LiteralPath $InstalledBinary -PathType Leaf)) {
        throw "Refusing to replace non-file install target: $InstalledBinary"
    }
    $attributes = (Get-Item -LiteralPath $InstalledBinary -Force).Attributes
    if (($attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Refusing to replace reparse-point install target: $InstalledBinary"
    }
    $existingVersion = (& $InstalledBinary version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $existingVersion) {
        throw "Could not identify the existing Repo Sync installation; it was not replaced."
    }
    Write-Host "Stopping and disabling existing Repo Sync $existingVersion before upgrade..."
    & $InstalledBinary stop | Out-Host
    if ($LASTEXITCODE -ne 0) {
        throw "Existing Repo Sync could not be stopped; the upgrade was not installed."
    }
    if ($existingVersion -match '^v?0\.1\.') {
        Wait-LegacyLocksReleased (Join-Path $env:APPDATA "repo-sync")
    }
    return $true
}

New-Item -ItemType Directory -Path $temporaryDir | Out-Null
try {
    Invoke-WebRequest "$baseUrl/$archive" -OutFile (Join-Path $temporaryDir $archive)
    Invoke-WebRequest "$baseUrl/SHA256SUMS" -OutFile (Join-Path $temporaryDir "SHA256SUMS")
    $checksumLine = Get-Content (Join-Path $temporaryDir "SHA256SUMS") | Where-Object { $_ -match "\s$([regex]::Escape($archive))$" }
    if (-not $checksumLine) { throw "The release checksum does not contain $archive." }
    $expected = ($checksumLine -split "\s+")[0].ToLowerInvariant()
    $actual = (Get-FileHash (Join-Path $temporaryDir $archive) -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) { throw "Checksum verification failed for $archive." }

    Expand-Archive (Join-Path $temporaryDir $archive) -DestinationPath $temporaryDir
    $staged = Join-Path $temporaryDir "repo-sync.exe"
    $reportedVersion = (& $staged version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $reportedVersion -ne $Version) {
        throw "The staged executable reported version '$reportedVersion'; expected '$Version'."
    }

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    if ((Test-Path -LiteralPath $candidate) -or (Test-Path -LiteralPath $backup)) {
        throw "Refusing to overwrite an existing installer recovery file."
    }
    $upgraded = Stop-ExistingRepoSync $installedBinary
    Copy-Item -LiteralPath $staged -Destination $candidate
    if (Test-Path -LiteralPath $installedBinary) {
        Move-Item -LiteralPath $installedBinary -Destination $backup
    }
    try {
        Move-Item -LiteralPath $candidate -Destination $installedBinary
        $installedVersion = (& $installedBinary version | Out-String).Trim()
        if ($LASTEXITCODE -ne 0 -or $installedVersion -ne $Version) {
            throw "The installed executable reported version '$installedVersion'; expected '$Version'."
        }
    }
    catch {
        Remove-Item -LiteralPath $candidate -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $installedBinary -Force -ErrorAction SilentlyContinue
        if (Test-Path -LiteralPath $backup) {
            Move-Item -LiteralPath $backup -Destination $installedBinary
        }
        throw
    }
    Remove-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue
    Write-Host "Installed repo-sync $Version to $installedBinary"
    if ($upgraded) {
        Write-Host "The upgraded service remains disabled. Reconcile setup, then run repo-sync start."
    }
    if ($env:REPO_SYNC_NO_PATH -ne "1") {
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $entries = @($userPath -split ";" | Where-Object { $_ })
        $alreadyPresent = $entries | Where-Object { [string]::Equals($_.TrimEnd("\"), $InstallDir.TrimEnd("\"), [System.StringComparison]::OrdinalIgnoreCase) }
        if (-not $alreadyPresent) {
            [Environment]::SetEnvironmentVariable("Path", ((@($entries) + $InstallDir) -join ";"), "User")
            Write-Host "Added $InstallDir to your user PATH. Open a new terminal to use it."
        }
    }
    if (($env:Path -split ";") -notcontains $InstallDir) {
        Write-Host "Add $InstallDir to PATH in this shell to run repo-sync immediately."
    }
}
finally {
    Remove-Item -LiteralPath $temporaryDir -Recurse -Force -ErrorAction SilentlyContinue
}
