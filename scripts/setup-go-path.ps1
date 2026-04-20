# One-time Go PATH setup (no admin required — user PATH only).
# Run once in PowerShell: .\scripts\setup-go-path.ps1
# Then restart your terminal for the change to take effect.

$goBin = Join-Path $env:USERPROFILE "go-sdk\go\bin"

if (-not (Test-Path $goBin)) {
    Write-Error "Go not found at $goBin. Did the install step fail?"
    exit 1
}

$userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
if ($userPath -like "*$goBin*") {
    Write-Host "OK — $goBin already in user PATH."
} else {
    $newPath = if ($userPath) { "$userPath;$goBin" } else { $goBin }
    [Environment]::SetEnvironmentVariable("PATH", $newPath, "User")
    Write-Host "Added $goBin to user PATH."
    Write-Host "Restart your terminal (or log out/in) to pick it up."
}

# Sanity check via absolute path.
& "$goBin\go.exe" version
