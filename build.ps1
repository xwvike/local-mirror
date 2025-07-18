# Go Cross-Platform Build Script
# Builds binaries for multiple platforms

param(
    [string]$Output = "dist"
)

$AppName = "local-mirror"
$MainPath = "./cmd/local-mirror"

Write-Host "=== Go Cross-Platform Build Script ===" -ForegroundColor Green
Write-Host "Project: $AppName" -ForegroundColor Yellow
Write-Host "Output: $Output" -ForegroundColor Yellow

# Clean and create output directory
if (Test-Path $Output) {
    Write-Host "Cleaning old builds..." -ForegroundColor Cyan
    Remove-Item -Recurse -Force $Output
}
New-Item -ItemType Directory -Path $Output | Out-Null

# Define build targets
$targets = @(
    @{OS="windows"; Arch="amd64"; Name="Windows 64-bit"},
    @{OS="windows"; Arch="386"; Name="Windows 32-bit"},
    @{OS="linux"; Arch="amd64"; Name="Linux 64-bit"},
    @{OS="linux"; Arch="arm64"; Name="Linux ARM64"},
    @{OS="linux"; Arch="arm"; Name="Linux ARM"},
    @{OS="darwin"; Arch="amd64"; Name="macOS Intel"},
    @{OS="darwin"; Arch="arm64"; Name="macOS Apple Silicon"}
)

Write-Host ""
Write-Host "Starting build..." -ForegroundColor Green

foreach ($target in $targets) {
    $ext = if ($target.OS -eq "windows") { ".exe" } else { "" }
    $outputFile = "$Output/$AppName-$($target.OS)-$($target.Arch)$ext"
    
    Write-Host "Building $($target.Name)..." -ForegroundColor Cyan
    
    $env:GOOS = $target.OS
    $env:GOARCH = $target.Arch
    $env:CGO_ENABLED = "0"
    
    try {
        $result = go build -ldflags "-s -w" -o $outputFile $MainPath 2>&1
        if ($LASTEXITCODE -eq 0) {
            $size = [math]::Round((Get-Item $outputFile).Length / 1MB, 2)
            Write-Host "  Success ($size MB)" -ForegroundColor Green
        } else {
            Write-Host "  Failed: $result" -ForegroundColor Red
        }
    } catch {
        Write-Host "  Error: $_" -ForegroundColor Red
    }
}

# Clean environment variables
Remove-Item Env:GOOS -ErrorAction SilentlyContinue
Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue

Write-Host ""
Write-Host "=== Build Complete ===" -ForegroundColor Green
Write-Host "Build Results:" -ForegroundColor Yellow

if (Test-Path $Output) {
    Get-ChildItem $Output | ForEach-Object {
        $sizeMB = [math]::Round($_.Length / 1MB, 2)
        Write-Host "  $($_.Name) - $sizeMB MB" -ForegroundColor White
    }
    
    $totalSize = [math]::Round((Get-ChildItem $Output | Measure-Object -Property Length -Sum).Sum / 1MB, 2)
    Write-Host ""
    Write-Host "Total size: $totalSize MB" -ForegroundColor Yellow
} else {
    Write-Host "  No files generated" -ForegroundColor Red
}
