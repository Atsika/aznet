$f = 'C:\ProgramData\VMware\vmnetnat.conf'
if (-not (Test-Path "$f.proxylab-backup")) { Copy-Item $f "$f.proxylab-backup" }
$t = [System.IO.File]::ReadAllText($f)
if ($t -notmatch '(?m)^88 = 192\.168\.11\.10:88') {
  $t = $t -replace '(?m)^\[incomingtcp\]\r?\n', "[incomingtcp]`r`n88 = 192.168.11.10:88 # proxylab`r`n"
  $t = $t -replace '(?m)^\[incomingudp\]\r?\n', "[incomingudp]`r`n88 = 192.168.11.10:88 # proxylab`r`n"
  [System.IO.File]::WriteAllText($f, $t, (New-Object System.Text.UTF8Encoding($false)))
}
Get-Content $f | Select-String -Pattern '^\[incoming|^88 = ' | ForEach-Object { $_.Line }
if (-not (Get-NetFirewallRule -DisplayName 'proxylab-kdc-forward' -ErrorAction SilentlyContinue)) {
  New-NetFirewallRule -DisplayName 'proxylab-kdc-forward' -Direction Inbound -Action Allow -Protocol TCP -LocalPort 88 -RemoteAddress 192.168.1.0/24,100.64.0.0/10 | Out-Null
  New-NetFirewallRule -DisplayName 'proxylab-kdc-forward-udp' -Direction Inbound -Action Allow -Protocol UDP -LocalPort 88 -RemoteAddress 192.168.1.0/24,100.64.0.0/10 | Out-Null
}
Restart-Service 'VMware NAT Service' -Force
Start-Sleep 5
Get-Service 'VMware NAT Service' | ForEach-Object { "NAT service: " + $_.Status }
Get-NetTCPConnection -LocalPort 88 -State Listen -ErrorAction SilentlyContinue | Select-Object LocalAddress, LocalPort | Format-Table -Auto | Out-String
