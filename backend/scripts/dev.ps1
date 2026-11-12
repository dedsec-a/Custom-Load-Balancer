# Build and run 3 backends + the load balancer locally.
#   .\scripts\dev.ps1                 # default: 3 backends, 2 cores / 5 DB conns each
#   .\scripts\dev.ps1 -Backends 4 -Cores 4 -DBPool 10
# Ctrl+C stops everything.
param(
    [int]$Backends = 3,
    [int]$Cores = 2,
    [int]$DBPool = 5,
    [string]$Algorithm = "auto"
)
$ErrorActionPreference = "Stop"
$root = Split-Path $PSScriptRoot -Parent
Set-Location $root

go build -o bin/backend.exe ./cmd/backend
go build -o bin/lb.exe ./cmd/lb

$procs = @()
$list = @()
try {
    for ($i = 1; $i -le $Backends; $i++) {
        $port = 3000 + $i
        $procs += Start-Process -PassThru -NoNewWindow bin/backend.exe `
            -ArgumentList "--port $port --name b$i --cores $Cores --db-pool $DBPool"
        $list += "b$i=http://127.0.0.1:$port"
    }
    Start-Sleep -Milliseconds 500
    Write-Host "Traffic: http://localhost:8080   Admin API: http://localhost:8081/api/state"
    & bin/lb.exe --backends ($list -join ",") --algorithm $Algorithm --log text --results-dir results/experiments
}
finally {
    $procs | ForEach-Object { if (-not $_.HasExited) { Stop-Process -Id $_.Id -Force } }
}
