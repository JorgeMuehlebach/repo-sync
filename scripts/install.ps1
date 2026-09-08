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
$destination = Join-Path $InstallDir "repo-sync.exe"
$candidate = "$destination.new"
$backup = "$destination.previous"
$wasRunning = $false
$stopped = $false

function Restore-PreviousVersion {
    Remove-Item -LiteralPath $candidate -Force -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $backup) {
        Remove-Item -LiteralPath $destination -Force -ErrorAction SilentlyContinue
        Move-Item -LiteralPath $backup -Destination $destination -Force
    }
    if ($wasRunning -and (Test-Path -LiteralPath $destination)) {
        try { & $destination start | Out-Host } catch { Write-Warning "The previous service could not be restarted: $_" }
    }
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
    $reportedVersion = ((& $staged version) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $reportedVersion -ne $Version) {
        throw "The staged executable reported version '$reportedVersion'; expected '$Version'."
    }

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Copy-Item -LiteralPath $staged -Destination $candidate -Force
    if (Test-Path -LiteralPath $destination) {
        $statusOutput = ((& $destination status 2>&1) | Out-String)
        $wasRunning = $LASTEXITCODE -eq 0 -and $statusOutput -match "(?m)^Service:\s+running\s*$"
        if ($wasRunning) {
            & $destination stop | Out-Host
            if ($LASTEXITCODE -ne 0) { throw "Could not stop the running Repo Sync service." }
            $stopped = $true
        }
        Remove-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue
        Move-Item -LiteralPath $destination -Destination $backup
    }
    try {
        Move-Item -LiteralPath $candidate -Destination $destination
        $installedVersion = ((& $destination version) | Out-String).Trim()
        if ($LASTEXITCODE -ne 0 -or $installedVersion -ne $Version) {
            throw "The installed executable reported version '$installedVersion'; expected '$Version'."
        }
        if ($wasRunning) {
            & $destination start | Out-Host
            if ($LASTEXITCODE -ne 0) { throw "The upgraded Repo Sync service did not restart." }
        }
    }
    catch {
        Restore-PreviousVersion
        $stopped = $false
        throw
    }
    Remove-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue

    if ($env:REPO_SYNC_NO_PATH -ne "1") {
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $entries = @($userPath -split ";" | Where-Object { $_ })
        $alreadyPresent = $entries | Where-Object { [string]::Equals($_.TrimEnd("\"), $InstallDir.TrimEnd("\"), [System.StringComparison]::OrdinalIgnoreCase) }
        if (-not $alreadyPresent) {
            $updatedPath = (@($entries) + $InstallDir) -join ";"
            [Environment]::SetEnvironmentVariable("Path", $updatedPath, "User")
            Write-Host "Added $InstallDir to your user PATH. Open a new terminal to use it."
        }
        if (($env:Path -split ";") -notcontains $InstallDir) { $env:Path = "$InstallDir;$env:Path" }
    }
    Write-Host "Installed repo-sync $Version to $destination"
}
catch {
    if ($stopped) {
        Restore-PreviousVersion
        $stopped = $false
    }
    throw
}
finally {
    Remove-Item -LiteralPath $temporaryDir -Recurse -Force -ErrorAction SilentlyContinue
}
