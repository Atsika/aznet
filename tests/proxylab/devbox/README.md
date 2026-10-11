# Scripts for a VMware host

For reaching a VMware NAT guest's Kerberos port from another machine on the LAN, without
bridging the VM or changing its network.

- `kdc-forward.ps1` (run elevated on the VMware host): forwards TCP and UDP 88 to the
  domain controller (192.168.11.10 here; edit the address) through VMware's NAT, opens the
  host firewall for the LAN and Tailscale ranges, and restarts the NAT service. It backs
  up `vmnetnat.conf` first.
- `kdc-forward-revert.ps1`: restores the backup, removes the firewall rules, restarts NAT.

Then `go run ./tests/proxylab/adprobe <host>:88 REALM user password [spn]` from another
machine shows whether a Unix client (gokrb5, the library aznet uses) can log in and get a
service ticket from that KDC.

Untested end to end: the forward was in place when the domain controller restarted on its
own, so no traffic reached it through the forward.
