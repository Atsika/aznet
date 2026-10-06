---
title: Security and Authorization
description: Encryption, anonymous peers, credential scope, and resource ownership.
---

aznet uses `Noise_NN_25519_AESGCM_SHA256`. Application frames and the session-token response are encrypted; **NN does not authenticate peer identity**. Ephemeral key agreement and AEAD integrity alone do not protect an anonymous handshake against an active intermediary able to replace its messages. Applications needing peer authentication must supply it at an appropriate layer. See the [Noise pattern specification](https://noiseprotocol.org/noise.html#handshake-patterns).

## Visible metadata

The first handshake carries the client's UUID without encryption. Resource names, message lengths, ciphertext lengths and traffic timing are also visible to the storage service. A sealed chunk has a four-byte length prefix and a 16-byte authentication tag in addition to encrypted content. Do not describe all stored content or metadata as secret.

## Credential scope

The listener uses account credentials to provision resources and issue SAS tokens. Clients receive bootstrap credentials and, after the handshake, session credentials; the listener does not send its account key in the connection URL.

| Service | Bootstrap handshake / token permissions | Client session request / response permissions |
| :--- | :--- | :--- |
| Blob | Add/Create/Write / Read/List | Same container SAS for both: Read/List/Add/Create/Write |
| Queue | Add / Read | Add / Read/Process |
| Table | Add / Read | Add / Read/Delete |

Session resources are named using the client's validated UUID. Resource scoping reduces cross-session access, but Blob's container token does not isolate the two directions. Bootstrap credentials are bearer secrets for a shared namespace, not proof of a particular peer's identity. Protect them accordingly.

## Lifetime and cleanup

Bootstrap and session credentials default to 24 hours and have independent [configuration](/reference/options#independent-credential-lifetimes). `ConnectionStringFor` changes only that URL's issuance duration. There is no automatic credential refresh. Bootstrap expiry prevents later joining; it does not itself expire sessions already established. `SessionExpiry` reports issuance metadata, not liveness.

The listener owns accepted-session cleanup. Its janitor closes idle sessions while the process runs; it cannot reclaim every resource left by a crash or an unavailable backend. Close errors and timeouts require follow-up. Shared bootstrap deletion is an explicit administrator operation after every namespace user stops.

## Deployment

Use HTTPS to Azure, protect account keys and connection URLs, and configure storage network access for the participating endpoints. Anonymous public Blob access is not required. Avoid logging credentials or raw SDK errors without redaction: an error may include request details. Validate authentication and cleanup behavior in the intended host; compilation alone is not runtime evidence.
