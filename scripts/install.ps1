$ErrorActionPreference = "Stop"

$repository = "JorgeMuehlebach/repo-sync"
$release = Invoke-RestMethod "https://api.github.com/repos/$repository/releases/latest"
$version = $release.tag_name.TrimStart("v")
$architecture = if ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq "Arm64") { "arm64" } else { "amd64" }
$archive = "repo-sync_${version}_windows_${architecture}.zip"
$baseUrl = "https://github.com/$repository/releases/download/v$version"
$temporaryDir = Join-Path ([System.IO.Path]::GetTempPath()) ("repo-sync-" + [System.Guid]::NewGuid())
$installDir = if ($env:REPO_SYNC_INSTALL_DIR) { $env:REPO_SYNC_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\repo-sync" }

New-Item -ItemType Directory -Path $temporaryDir | Out-Null
try {
    Invoke-WebRequest "$baseUrl/$archive" -OutFile (Join-Path $temporaryDir $archive)
    Invoke-WebRequest "$baseUrl/SHA256SUMS" -OutFile (Join-Path $temporaryDir "SHA256SUMS")
    $checksumLine = Get-Content (Join-Path $temporaryDir "SHA256SUMS") | Where-Object { $_ -match "\s$([regex]::Escape($archive))$" }
    if (-not $checksumLine) { throw "The release checksum does not contain $archive." }
    $expected = ($checksumLine -split "\s+")[0].ToLowerInvariant()
    $actual = (Get-FileHash (Join-Path $temporaryDir $archive) -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) { throw "Checksum verification failed." }

    Expand-Archive (Join-Path $temporaryDir $archive) -DestinationPath $temporaryDir
    New-Item -ItemType Directory -Force -Path $installDir | Out-Null
    Copy-Item (Join-Path $temporaryDir "repo-sync.exe") (Join-Path $installDir "repo-sync.exe") -Force
    Write-Host "Installed repo-sync $version to $installDir\repo-sync.exe"
    if (($env:Path -split ";") -notcontains $installDir) {
        Write-Host "Add $installDir to PATH to run repo-sync."
    }
}
finally {
    Remove-Item -Recurse -Force $temporaryDir -ErrorAction SilentlyContinue
}
