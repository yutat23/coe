# wingetパッケージ用ビルドスクリプト
$appname = "coe"
$version = "0.1.0"

# リリースディレクトリを作成
$releaseDir = "release"
if (!(Test-Path $releaseDir)) {
    New-Item -ItemType Directory -Path $releaseDir -Force | Out-Null
}

# Windows用のビルド
@("amd64", "arm64") | ForEach-Object {
    $env:GOOS = "windows"
    $env:GOARCH = $_
    $target = "windows_$env:GOARCH"
    $buildDir = "build/$appname" + "_$target"
    $outputFile = "$buildDir/$appname.exe"

    Write-Host "Building for $target..." -ForegroundColor Green
    
    # ビルドディレクトリを作成
    if (Test-Path $buildDir) {
        Remove-Item $buildDir -Recurse -Force
    }
    New-Item -ItemType Directory -Path $buildDir -Force | Out-Null

    # Goアプリケーションをビルド
    go build -o $outputFile -ldflags "-s -w" main.go

    # インストーラースクリプトをコピー
    Copy-Item "installer.ps1" "$buildDir/"

    # ZIP化
    $zipPath = "$releaseDir/$appname" + "_$version" + "_$target.zip"
    if (Test-Path $zipPath) { 
        Remove-Item $zipPath -Force 
    }
    
    Write-Host "Creating ZIP: $zipPath" -ForegroundColor Yellow
    Compress-Archive -Path (Get-ChildItem -Path $buildDir) -DestinationPath $zipPath

    # SHA256ハッシュを計算
    $hash = Get-FileHash -Path $zipPath -Algorithm SHA256
    Write-Host "SHA256 for $target`: $($hash.Hash)" -ForegroundColor Cyan
}

# Linux用のビルド
@("amd64", "arm64") | ForEach-Object {
    $env:GOOS = "linux"
    $env:GOARCH = $_
    $target = "linux_$env:GOARCH"
    $buildDir = "build/$appname" + "_$target"
    $outputFile = "$buildDir/$appname"

    Write-Host "Building for $target..." -ForegroundColor Green
    
    # ビルドディレクトリを作成
    if (Test-Path $buildDir) {
        Remove-Item $buildDir -Recurse -Force
    }
    New-Item -ItemType Directory -Path $buildDir -Force | Out-Null

    # Goアプリケーションをビルド
    go build -o $outputFile -ldflags "-s -w" main.go

    # ZIP化
    $zipPath = "$releaseDir/$appname" + "_$version" + "_$target.zip"
    if (Test-Path $zipPath) { 
        Remove-Item $zipPath -Force 
    }
    
    Write-Host "Creating ZIP: $zipPath" -ForegroundColor Yellow
    Compress-Archive -Path (Get-ChildItem -Path $buildDir) -DestinationPath $zipPath

    # SHA256ハッシュを計算
    $hash = Get-FileHash -Path $zipPath -Algorithm SHA256
    Write-Host "SHA256 for $target`: $($hash.Hash)" -ForegroundColor Cyan
}

# macOS用のビルド
@("amd64", "arm64") | ForEach-Object {
    $env:GOOS = "darwin"
    $env:GOARCH = $_
    $target = "darwin_$env:GOARCH"
    $buildDir = "build/$appname" + "_$target"
    $outputFile = "$buildDir/$appname"

    Write-Host "Building for $target..." -ForegroundColor Green
    
    # ビルドディレクトリを作成
    if (Test-Path $buildDir) {
        Remove-Item $buildDir -Recurse -Force
    }
    New-Item -ItemType Directory -Path $buildDir -Force | Out-Null

    # Goアプリケーションをビルド
    go build -o $outputFile -ldflags "-s -w" main.go

    # ZIP化
    $zipPath = "$releaseDir/$appname" + "_$version" + "_$target.zip"
    if (Test-Path $zipPath) { 
        Remove-Item $zipPath -Force 
    }
    
    Write-Host "Creating ZIP: $zipPath" -ForegroundColor Yellow
    Compress-Archive -Path (Get-ChildItem -Path $buildDir) -DestinationPath $zipPath

    # SHA256ハッシュを計算
    $hash = Get-FileHash -Path $zipPath -Algorithm SHA256
    Write-Host "SHA256 for $target`: $($hash.Hash)" -ForegroundColor Cyan
}

Write-Host "Build completed! Files are in the $releaseDir directory." -ForegroundColor Green
