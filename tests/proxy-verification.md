# Proxy Verification Test Plan

> **Status (macOS):** the scenarios that can run without Windows are automated
> in `tests/proxylab` (Samba AD + domain-joined Squid proxies; see its README)
> and pass. They cover Basic, NTLM, Negotiate with a `kinit` file cache and with
> a password login, `ProxyAuto` ordering and the Negotiate-to-NTLM fallback,
> wrong/disabled/missing credentials, and a rejected login being tried once.
> Still to run by hand: scenarios 6, 7, 12 and 15 (Windows), and the same lab on Linux.

Verification scenarios for the egress proxy feature across platforms.
Each scenario uses a local test proxy and the echo example against Azurite.

## Prerequisites

All platforms:
- Docker (for Azurite): `docker-compose up`
- Go 1.26+ installed

Platform-specific:
- **Windows:** Domain-joined machine or local NTLM proxy for SSPI tests
- **Linux:** `krb5-user` package, a KDC (MIT or FreeIPA) for Kerberos tests
- **macOS:** Heimdal kinit, or skip Kerberos scenarios

## Test Proxy Setup

Use [mitmproxy](https://mitmproxy.org/) for Basic auth scenarios and
[px-proxy](https://github.com/genotrance/px) or a Squid instance for
NTLM/Negotiate scenarios.

### Quick Basic-auth proxy (any platform)

```bash
# mitmproxy with Basic auth
mitmproxy --proxyauth "testuser:testpass" -p 8888 --mode regular
```

### Squid with NTLM (Linux/Docker)

```bash
docker run -d --name squid-ntlm -p 3128:3128 \
  -v $(pwd)/tests/proxy-squid.conf:/etc/squid/squid.conf \
  ubuntu/squid
```

---

## Scenarios

### 1. Direct connection (no proxy) — baseline

**Platforms:** all  
**Purpose:** confirm nothing is broken when no proxy option is set

```bash
docker-compose up -d
# Terminal 1
cd examples/echo && go run ./server/
# Terminal 2 (paste the connection string)
cd examples/echo && go run ./client/
```

**Expected:** echo works, no proxy log output.

---

### 2. Basic auth — explicit proxy

**Platforms:** Linux, macOS, Windows  
**Purpose:** `WithProxy` with `ProxyBasic`

```go
opts = append(opts,
    aznet.WithProxy("http://127.0.0.1:8888", aznet.ProxyBasic, "testuser", "testpass"),
    aznet.WithProxyLog(log.Printf),
)
```

**Expected:** proxy log shows:
- `CONNECT round 1, sending Basic`
- `proxy replied 200`
- `tunnel established`

---

### 3. Basic auth — credentials in URL

**Platforms:** all  
**Purpose:** credentials extracted from proxy URL

```go
opts = append(opts,
    aznet.WithProxy("http://testuser:testpass@127.0.0.1:8888", aznet.ProxyAuto, "", ""),
    aznet.WithProxyLog(log.Printf),
)
```

**Expected:** same as scenario 2.

---

### 4. ProxyAuto — Basic-only proxy

**Platforms:** all  
**Purpose:** `ProxyAuto` negotiates down to Basic when that's all the proxy offers

```go
opts = append(opts,
    aznet.WithProxy("http://127.0.0.1:8888", aznet.ProxyAuto, "testuser", "testpass"),
    aznet.WithProxyLog(log.Printf),
)
```

**Expected:** proxy log shows:
- `proxy offers basic`
- `answering with basic`
- `tunnel established`

---

### 5. NTLM — explicit credentials

**Platforms:** all (Squid with NTLM helper)  
**Purpose:** three-message NTLM exchange over CONNECT

```go
opts = append(opts,
    aznet.WithProxy("http://127.0.0.1:3128", aznet.ProxyNTLM, "DOMAIN\\user", "pass"),
    aznet.WithProxyLog(log.Printf),
)
```

**Expected:** proxy log shows:
- `CONNECT round 1, sending no credentials`
- `proxy offers ntlm`
- `answering with ntlm`
- `CONNECT round 2, sending NTLM` (negotiate message)
- `CONNECT round 3, sending NTLM` (authenticate message)
- `tunnel established`

---

### 6. NTLM — ambient credentials (Windows only)

**Platforms:** Windows  
**Purpose:** SSPI authenticates as the logged-on user

```go
opts = append(opts,
    aznet.WithProxy("http://proxy.corp:8080", aznet.ProxyNTLM, "", ""),
    aznet.WithProxyLog(log.Printf),
)
```

**Expected:** tunnel established, no credentials in log output.

---

### 7. Negotiate/Kerberos — Windows SSPI

**Platforms:** Windows (domain-joined)  
**Purpose:** SPNEGO via SSPI, Kerberos preferred

```go
opts = append(opts,
    aznet.WithProxy("http://proxy.corp:8080", aznet.ProxyNegotiate, "", ""),
    aznet.WithProxyLog(log.Printf),
)
```

**Expected:** `answering with negotiate`, tunnel established.

---

### 8. Negotiate/Kerberos — Linux with kinit

**Platforms:** Linux  
**Purpose:** gokrb5 reads the file-based ticket cache

```bash
kinit user@REALM
export KRB5CCNAME=FILE:/tmp/krb5cc_$(id -u)
```

```go
opts = append(opts,
    aznet.WithProxy("http://proxy.corp:8080", aznet.ProxyNegotiate, "", ""),
    aznet.WithProxyLog(log.Printf),
)
```

**Expected:** `answering with negotiate`, tunnel established.

---

### 9. Negotiate/Kerberos — Linux with password

**Platforms:** Linux  
**Purpose:** gokrb5 authenticates with explicit credentials

```go
opts = append(opts,
    aznet.WithProxy("http://proxy.corp:8080", aznet.ProxyNegotiate, "user@REALM", "password"),
    aznet.WithProxyLog(log.Printf),
)
```

**Expected:** `answering with negotiate`, tunnel established.

---

### 10. macOS — Kerberos cache not found

**Platforms:** macOS  
**Purpose:** clear error when no file-based cache exists

```bash
unset KRB5CCNAME
```

```go
opts = append(opts,
    aznet.WithProxy("http://proxy:8080", aznet.ProxyNegotiate, "", ""),
    aznet.WithProxyLog(log.Printf),
)
```

**Expected:** error contains `no file-based Kerberos ticket cache found; run "kinit"`.

---

### 11. macOS — Kerberos with explicit kinit

**Platforms:** macOS  
**Purpose:** kinit writes a file cache that gokrb5 can read

```bash
KRB5CCNAME=FILE:/tmp/krb5cc_test kinit user@REALM
export KRB5CCNAME=FILE:/tmp/krb5cc_test
```

**Expected:** `answering with negotiate`, tunnel established.

---

### 12. ProxyAuto — Negotiate fails, falls back to NTLM

**Platforms:** Windows (or any where Negotiate can be attempted)  
**Purpose:** the new respond-time fallback

Set up a proxy that offers both Negotiate and NTLM but rejects the
Kerberos ticket (e.g., the SPN doesn't match).

**Expected:** proxy log shows:
- `answering with negotiate`
- `negotiate authentication failed`
- `falling back to ntlm`
- `tunnel established`

---

### 13. WithProxyFunc — environment-based resolution

**Platforms:** all  
**Purpose:** ProxyBlob's `resolveProxy` picks up env vars

```bash
HTTPS_PROXY=http://testuser:testpass@127.0.0.1:8888 go run ./cmd/agent/ -c "..."
```

**Expected:** traffic goes through the proxy.

---

### 14. WithProxyFunc — NO_PROXY bypass

**Platforms:** all  
**Purpose:** connections to bypassed hosts go direct

```bash
HTTPS_PROXY=http://127.0.0.1:8888 NO_PROXY=127.0.0.1 go run ./cmd/agent/ -c "..."
```

**Expected:** direct connection (no proxy log).

---

### 15. Windows system proxy — Internet Options

**Platforms:** Windows  
**Purpose:** `go-ieproxy` reads the registry / WinHTTP settings

Set a proxy in Internet Options → LAN Settings, then run the agent
without any environment variables.

**Expected:** traffic goes through the configured proxy.

---

### 16. Invalid proxy — clear error

**Platforms:** all  
**Purpose:** bad proxy address produces a useful error

```go
opts = append(opts,
    aznet.WithProxy("socks5://127.0.0.1:1080", aznet.ProxyAuto, "", ""),
)
```

**Expected:** error contains `unsupported scheme "socks5"`.

---

### 17. Proxy down — connection error

**Platforms:** all  
**Purpose:** unreachable proxy produces a clear error

```go
opts = append(opts,
    aznet.WithProxy("http://127.0.0.1:19999", aznet.ProxyBasic, "u", "p"),
    aznet.WithProxyLog(log.Printf),
)
```

**Expected:** error wraps `ErrProxyConnect`, log shows `cannot reach the proxy`.

---

### 18. WithProxyLog — no credentials leaked

**Platforms:** all  
**Purpose:** verify no passwords, tokens, or signatures appear in log output

Run any authenticated scenario with `WithProxyLog` and capture the output.

**Expected:** log mentions scheme names, status codes, and proxy addresses
only. No `testpass`, no base64-encoded credentials, no Authorization header
values beyond the scheme name.

---

## Automated Verification

The unit tests in `proxy_test.go` and `proxy_negotiate_unix_test.go` cover:
- CONNECT tunnel establishment through a fake proxy
- Basic, NTLM, and ProxyAuto scheme selection
- 407 challenge parsing and multi-round exchanges
- Proxy reconnection when the server closes the connection
- Credential leak prevention in logs
- Kerberos cache path resolution (FILE, KEYRING, KCM, API, default)
- Negotiate-to-NTLM fallback when Kerberos is unavailable
- Invalid/unsupported proxy schemes

Run: `go test -race -count=1 -v -run TestProxy ./...`

## Results Matrix

| # | Scenario | macOS | Linux | Windows |
|---|----------|-------|-------|---------|
| 1 | Direct | | | |
| 2 | Basic explicit | | | |
| 3 | Basic URL creds | | | |
| 4 | ProxyAuto Basic | | | |
| 5 | NTLM explicit | | | |
| 6 | NTLM ambient | N/A | N/A | |
| 7 | Negotiate SSPI | N/A | N/A | |
| 8 | Negotiate kinit | N/A | | N/A |
| 9 | Negotiate password | N/A | | N/A |
| 10 | macOS no cache | | N/A | N/A |
| 11 | macOS kinit | | N/A | N/A |
| 12 | Negotiate→NTLM | | | |
| 13 | Env proxy | | | |
| 14 | NO_PROXY | | | |
| 15 | System proxy | N/A | N/A | |
| 16 | Invalid scheme | | | |
| 17 | Proxy down | | | |
| 18 | No cred leak | | | |
