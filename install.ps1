$ErrorActionPreference = "Stop"

$Repo = "voidmind-io/voidmcp"
$InstallDir = if ($env:INSTALL_DIR) {
    $env:INSTALL_DIR
} else {
    Join-Path $HOME "AppData\Local\Programs\voidmcp\bin"
}

$Arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    default { throw "Unsupported architecture: $($env:PROCESSOR_ARCHITECTURE)" }
}

$Release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest"
$Version = $Release.tag_name
if (-not $Version) {
    throw "Failed to fetch latest version"
}

$Archive = "voidmcp-windows-$Arch.zip"
$Url = "https://github.com/$Repo/releases/download/$Version/$Archive"

Write-Host "Installing VoidMCP $Version (windows/$Arch)..."

$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("voidmcp-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $TempDir | Out-Null

try {
    $ZipPath = Join-Path $TempDir $Archive
    Invoke-WebRequest -Uri $Url -OutFile $ZipPath
    Expand-Archive -Path $ZipPath -DestinationPath $TempDir -Force

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    Copy-Item (Join-Path $TempDir "voidmcp.exe") (Join-Path $InstallDir "voidmcp.exe") -Force

    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $PathEntries = @()
    if ($UserPath) {
        $PathEntries = $UserPath -split ';' | Where-Object { $_ }
    }

    if ($PathEntries -notcontains $InstallDir) {
        $NewPath = if ($UserPath) { "$UserPath;$InstallDir" } else { $InstallDir }
        [Environment]::SetEnvironmentVariable("Path", $NewPath, "User")
        $env:Path = "$env:Path;$InstallDir"
        Write-Host "Added $InstallDir to your user PATH."
    }

    Write-Host "Installed to $InstallDir\voidmcp.exe"
    Write-Host ""
    Write-Host "Quick start:"
    Write-Host "  claude mcp add --transport stdio voidmcp -- voidmcp serve --stdio"
    Write-Host ""
    Write-Host "If this shell was open before installation, restart it if 'voidmcp' is not found yet."
}
finally {
    if (Test-Path $TempDir) {
        Remove-Item $TempDir -Recurse -Force
    }
}
