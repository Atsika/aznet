$f = 'C:\ProgramData\VMware\vmnetnat.conf'
if (Test-Path "$f.proxylab-backup") { Copy-Item "$f.proxylab-backup" $f -Force; Remove-Item "$f.proxylab-backup" }
"forward lines left: " + @(Select-String -Path $f -Pattern '^88 = ').Count
Get-NetFirewallRule -DisplayName 'proxylab-kdc-forward*' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
"firewall rules left: " + @(Get-NetFirewallRule -DisplayName 'proxylab-kdc-forward*' -ErrorAction SilentlyContinue).Count
Restart-Service 'VMware NAT Service' -Force
Start-Sleep 3
"NAT service: " + (Get-Service 'VMware NAT Service').Status
"listening on 88: " + @(Get-NetTCPConnection -LocalPort 88 -State Listen -ErrorAction SilentlyContinue).Count
