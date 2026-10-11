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

Not covered here, because they need Windows: SSPI Negotiate, ambient NTLM as the
logged-on user, and system proxy settings / PAC.

ProxyBlob has a matching test for its environment-variable detection:
`AZNET_PROXYLAB=1 go test -run TestProxyLab ./internal/bootstrap/`.
