# OIDC key rotation and verification-failure policy (ID-03)

ID-03 requires "OIDC Discovery、JWKS 密钥轮换和验证失败策略" — discovery, JWKS key rotation, and a
policy for verification failures. This document states what this server does, so that a rotation
at the IdP is an understood event rather than an outage with an unknown cause.

## Discovery

`auth.NewProvider` reads `<issuer>/.well-known/openid-configuration` at startup. Failure is not
fatal: the server starts, logs a warning, and retries discovery lazily per login attempt, rate
limited to one attempt per 30 seconds. That behaviour and its rationale live in
`internal/auth/offline.go` and are covered by ID-07.

## Key rotation

The client uses go-oidc's `RemoteKeySet`, which:

- fetches `<jwks_uri>` on the first verification;
- caches keys by `kid`;
- refetches the JWKS when it encounters a `kid` it does not have.

**Rotation therefore requires no action on this server and no restart.** The sequence at the IdP is:

1. Publish the new key alongside the old one in the JWKS.
2. Start signing with the new key.
3. Keep the old key published until every token it signed has expired.
4. Withdraw the old key.

Steps 1 and 2 are the part that matters for us: because the new key is published *before* it is
used, a client that fetches the JWKS at any point during the rotation sees both keys. This is why
the fake IdP's `RotateKey` publishes the previous key rather than dropping it — an IdP that drops
the old key immediately invalidates fresh tokens, which is a self-inflicted outage.

## Verification-failure policy

Every one of these is refused. None is downgraded, retried as unauthenticated, or allowed through
with reduced claims:

| Failure | Result |
| --- | --- |
| Signature does not match the named key | Refused |
| `kid` absent from the JWKS | Refused (after one refetch) |
| Signing key not published at all | Refused |
| `iss` does not match the configured issuer | Refused |
| `aud` does not contain the client id | Refused |
| Token expired | Refused |
| `nonce` does not match the authorization request | Refused |
| No `nonce` was issued for the flow | Refused |
| `id_token` missing from the token response | Refused |

Signature, issuer, audience and expiry are enforced by go-oidc's `Verifier`. The nonce check is
**ours**, because go-oidc deliberately leaves replay binding to the caller — a verifier that
accepted a valid-but-replayed ID token would be an authentication bypass, and it is the one check
a library will not do for you.

The nonce comparison uses `subtle.ConstantTimeCompare`. That is not strictly necessary against a
value an attacker must guess outright, but it costs nothing and removes the question.

## What a failed login looks like

The callback redirects to the error page with a message that distinguishes the cases an operator
can act on (IdP unreachable → check the network; verification failed → check the IdP's keys and
the client id) from those they cannot. Both are audited as `auth.login` with `result=failure` and
the failure class in the metadata.

## What is deliberately not done

- **No pinned keys.** Pinning would make rotation a coordinated redeploy on both sides, which for a
  self-hosted platform with an external IdP is a guaranteed outage. The trust anchor is TLS to the
  issuer, plus the issuer claim check.
- **No offline JWKS cache on disk.** The keys are re-fetched per process; a restart during an IdP
  outage falls back to the ID-07 behaviour (existing sessions keep working, new logins fail).
- **No key-revocation push.** Withdrawing a compromised key takes effect for *new* verification
  attempts. A process that already has the key cached keeps it until it refetches, which happens
  on an unknown kid or a process restart. This is a property of the library's cache and is stated
  here rather than papered over: rotating a compromised key is fast but not instantaneous, and
  revoking *sessions* (which is a database operation) is the immediate lever.

## Where this is verified

| Property | Test |
| --- | --- |
| Login works against the initial key | `TestID03LoginSurvivesKeyRotation` (first half) |
| Login works after the IdP rotates | `TestID03LoginSurvivesKeyRotation` |
| Existing sessions survive rotation | `TestID03ExistingSessionSurvivesRotation` |
| Full key withdrawal is followed | `TestID03LoginSurvivesFullKeyWithdrawal` |
| Nonce binding | `TestLoginSendsAndVerifiesNonce`, `TestMismatchedNonceIsRejected` |
| Nonce replay | `TestNonceReplayAcrossFlowsIsRejected` |
| Missing nonce | `TestCallbackWithoutNonceCookieIsRejected` |
| Forged state | `TestForgedStateRejected` |

The rotation tests were impossible before `fakeidp.RotateKey` existed: a single-key IdP makes
"the client follows rotation" indistinguishable from "the client cached the first JWKS and never
needed another". Signing with an *unpublished* key turns both rotation tests red, which is how the
coverage was confirmed to be real rather than nominal.
