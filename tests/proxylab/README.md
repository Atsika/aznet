# Proxy lab

A small corporate network for testing aznet's egress-proxy support end to end:

| Service | Role |
|---|---|
| `dc` | Samba Active Directory domain controller for `CORP.TEST` (the KDC, and the account store behind NTLM and Basic) |
| `proxy-basic` `:3128` | Squid, domain-joined, asks for Basic (checked against AD) |
| `proxy-ntlm` `:3129` | Squid, asks for NTLM (Samba `ntlm_auth`) |
| `proxy-negotiate` `:3130` | Squid, asks for Negotiate/Kerberos (`negotiate_kerberos_auth`, keytab for `HTTP/proxy-negotiate.corp.test`) |
| `proxy-all` `:3131` | Squid, offers Negotiate, NTLM and Basic, as a typical company proxy does |
| `azurite` | Storage emulator the proxies tunnel to |

Users, all with password `Passw0rd!2024`: `alice`, `bob`, and `carol` (disabled).
Squid allows CONNECT to 443 and Azurite's 10000-10002 only.

```bash
cd tests/proxylab
docker compose up -d --build          # the first start provisions the domain, ~1 minute
AZNET_PROXYLAB=1 go test -v -count=1 ./   # from tests/proxylab, or ./tests/proxylab/ from the repo root
docker compose down -v                # forget the domain
```

The tests need `kinit` on the host (Heimdal on macOS, MIT on Linux) and run it
against the KDC on `localhost:88` with `krb5.conf` from this directory. To get a
ticket cache by hand:

```bash
export KRB5_CONFIG=$PWD/krb5.conf KRB5CCNAME=FILE:/tmp/krb5cc_lab
kinit alice@CORP.TEST                 # password Passw0rd!2024
```

The proxies are reached as `localhost`, so scenarios that use Kerberos set
`ProxyConfig.SPN` to the proxy's real service name (`HTTP/proxy-all.corp.test`).
Real deployments reach the proxy by its host name and need no override.

## Linux

`./run-linux.sh` builds the tests for Linux and runs them in a Debian container on
the lab network, with MIT `kinit` and the default `/tmp/krb5cc_<uid>` cache. It
works from any host OS with Docker. (ProxyBlob's `internal/bootstrap` test runs the
same way: build it with `go test -c`, set `LAB_TEST_BIN`, and
`docker compose --profile test run --rm runner lab.test -test.v -test.run TestProxyLab`.)

## Windows

Windows is where the platform-specific code lives (SSPI Negotiate, ambient NTLM,
Internet Options / PAC), so it needs a real Windows client. The tests are plain
`go test` binaries; build them on another machine with
`GOOS=windows go test -c ./tests/proxylab/` and copy them over, so Go is not needed.

Reach the lab from the Windows machine with `AZNET_PROXYLAB_HOST=<lab machine address>`
(the ports are published on all interfaces; allow them through the firewall), or run
the lab with Docker Desktop on the Windows machine and leave the variable unset.

```powershell
$env:AZNET_PROXYLAB = "1"; $env:AZNET_PROXYLAB_HOST = "192.168.1.17"

# 1. Explicit credentials: Basic, NTLM (go-ntlmssp), and Negotiate cases that
#    do not need a KDC. Kerberos scenarios are skipped on Windows by default.
.\aznet-lab.test.exe -test.v -test.count=1

# 2. The identity of the running process, without a domain: runas /netonly gives
#    a process network credentials, which SSPI's "current user" then presents.
#    From a non-interactive session (SSH), netonly.exe does the same with the
#    password in NETONLY_PASS (build it: GOOS=windows go build ./tests/proxylab/netonly):
runas /netonly /user:CORP\alice "powershell -NoExit"      # password Passw0rd!2024
#    in the new window:
$env:AZNET_PROXYLAB = "1"; $env:AZNET_PROXYLAB_HOST = "192.168.1.17"; $env:AZNET_PROXYLAB_AMBIENT = "1"
.\aznet-lab.test.exe -test.v -test.count=1 -test.run "TestProxyLab/ambient"

# 3. System proxy settings (what a corporate laptop has). Set Settings > Network >
#    Proxy > manual to 192.168.1.17:3129 (or point a PAC file at a lab proxy), then,
#    still in the runas /netonly window, with no proxy environment variables:
$env:AZNET_PROXYLAB_SYSTEM = "1"
.\proxyblob-bootstrap.test.exe -test.v -test.count=1 -test.run "TestProxyLab/system"
```

Kerberos on Windows needs the client to find the `CORP.TEST` KDC (`ksetup /addkdc
CORP.TEST <lab address>` as administrator, then `AZNET_PROXYLAB_KDC=1`). The ambient
`runas /netonly` runs use NTLM unless Kerberos is also set up. Proxies are reached by
address, so Kerberos scenarios set the service name explicitly.

Windows SSPI Negotiate falls back to NTLM inside SPNEGO when it has no Kerberos
ticket. Squid's Kerberos helper rejects that, whereas many commercial proxies accept
it, so a failure of `ambient/negotiate` without Kerberos configured says more about
Squid than about aznet. Use the `all` proxy (3131), which also offers NTLM.

Run so far: steps 1, 2 and 3 above on a standalone Windows machine against this lab
(static Internet Options proxy and a PAC file included). Not run: Kerberos on Windows
(SSPI with a KDC) and a domain-joined client.

ProxyBlob has a matching test for its environment-variable detection:
`AZNET_PROXYLAB=1 go test -run TestProxyLab ./internal/bootstrap/`.
