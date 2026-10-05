# Installs the jev binary from the GitHub releases of bragamat/jevkit on Windows.
#
#   irm https://raw.githubusercontent.com/bragamat/jevkit/main/install.ps1 | iex
#
# $env:JEV_VERSION = "0.1.1" pins a version (default: the latest release).
# $env:JEV_INSTALL_DIR changes the target directory (default: %LOCALAPPDATA%\Programs\jev).
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

$repo = "bragamat/jevkit"
$dir = if ($env:JEV_INSTALL_DIR) { $env:JEV_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\jev" }

$cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch ($cpu) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    default { throw "install.ps1: unsupported architecture $cpu" }
}

$base = if ($env:JEV_VERSION) {
    "https://github.com/$repo/releases/download/v$($env:JEV_VERSION.TrimStart('v'))"
} else {
    "https://github.com/$repo/releases/latest/download"
}

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("jev-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    # The checksums file names the archive, so the latest version needs no API call.
    $sums = (Invoke-WebRequest -UseBasicParsing "$base/checksums.txt").Content
    if ($sums -is [byte[]]) { $sums = [System.Text.Encoding]::UTF8.GetString($sums) }
    $line = ($sums -split "`n") | Where-Object { $_ -match "_windows_$arch\.zip\s*$" } | Select-Object -First 1
    if (-not $line) { throw "install.ps1: no release archive for windows/$arch" }
    $want, $archive = $line.Trim() -split "\s+"

    Write-Host "Downloading $archive"
    $zip = Join-Path $tmp $archive
    Invoke-WebRequest -UseBasicParsing "$base/$archive" -OutFile $zip
    $got = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLower()
    if ($got -ne $want.ToLower()) { throw "install.ps1: checksum mismatch for $archive (got $got, want $want)" }

    Expand-Archive -Path $zip -DestinationPath $tmp -Force
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    $exe = Join-Path $dir "jev.exe"
    Copy-Item -Force (Join-Path $tmp "jev.exe") $exe
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

Write-Host "Installed $(& $exe --version) to $exe"
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not (($userPath -split ";") -contains $dir)) {
    [Environment]::SetEnvironmentVariable("Path", (($userPath, $dir) | Where-Object { $_ }) -join ";", "User")
    $env:Path = "$env:Path;$dir"
    Write-Host "Added $dir to your user PATH; open a new terminal to use jev."
}
if (-not $env:TYPESAFE_API_KEY) {
    Write-Host "Then set TYPESAFE_API_KEY (get a key at https://console.typesafe.ai)."
}
