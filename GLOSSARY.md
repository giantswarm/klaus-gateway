# klaus-gateway

The Slack front door of the agent platform. Each term below takes its name from the spec or
upstream project that defines it, cited after the definition; a term no source defines says so.

## Signing in through muster

**Link**:
The stored association between a Slack user and their muster identity, holding the refresh
token the gateway acts on their behalf with.
_Avoid_: Session, binding, account

**Refresh request**:
A token request with the `refresh_token` grant, exchanging a refresh token for a new id_token
and refresh token (RFC 6749 §6).
_Avoid_: Spend, renewal

**Present**:
To send a refresh token in a refresh request (RFC 9700 §4.14.2).
_Avoid_: Spend, use, burn

**Rotation**:
muster issuing a new refresh token on every refresh request and invalidating the one presented
(RFC 9700 §4.14.2).

**Reuse detection**:
muster revoking the whole token family when an invalidated refresh token is presented again
(RFC 9700 §4.14.2).
_Avoid_: Replay protection, theft detection

**Token family**:
Every refresh token descended by rotation from one sign-in. mcp-oauth's term; RFC 9700 has
none.
_Avoid_: Grant, lineage

**Refresh lease**:
One gateway process's expiring hold on a link's refresh token, so that no other process
presents the same token. No spec defines it; the name follows the Kubernetes `Lease` (a
holder and an expiry).
_Avoid_: Claim (a JWT claim in this context), lock

## Team reviews

**Review claim**:
The hold a click takes on an open team review, so that of two clicks only one decides it. The
same pattern as a refresh lease; it keeps "claim" because the stored review record names it
`claimed_at`.
_Avoid_: Lock
