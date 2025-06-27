# coe Installer Script for winget
param(
    [string]$InstallLocation = "$env:LOCALAPPDATA\Programs\coe"
)

# Create installation directory
if (!(Test-Path $InstallLocation)) {
    New-Item -ItemType Directory -Path $InstallLocation -Force | Out-Null
}

# Copy coe.exe to installation directory
$exePath = Join-Path $PSScriptRoot "coe.exe"
if (Test-Path $exePath) {
    Copy-Item $exePath $InstallLocation -Force
    Write-Host "coe has been installed to: $InstallLocation" -ForegroundColor Green
} else {
    Write-Error "coe.exe not found in the current directory"
    exit 1
}

# Add to PATH if not already present
$currentPath = [Environment]::GetEnvironmentVariable("PATH", "User")
if ($currentPath -notlike "*$InstallLocation*") {
    [Environment]::SetEnvironmentVariable("PATH", "$currentPath;$InstallLocation", "User")
    Write-Host "Added coe to PATH" -ForegroundColor Green
}

Write-Host "Installation completed successfully!" -ForegroundColor Green
Write-Host "You can now use 'coe' command from anywhere." -ForegroundColor Yellow 