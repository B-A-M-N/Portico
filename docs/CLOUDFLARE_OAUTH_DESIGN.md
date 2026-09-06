# Cloudflare OAuth credential acquisition (Phase 2 design)

**Status:** Proposed — not implemented. Phase 1 (provider-declared help URL
with a launch-browser action) is shipped; this document describes what comes
after it.

## What this buys

Today the first Cloudflare account requires pasting an API token. Phase 2
replaces that with the standard OAuth2 authorization-code flow: Portico opens
the browser, the user signs in to Cloudflare however they normally do
(Google SSO included — sign-in method is the dashboard's concern), approves a
scope consent, and Cloudflare redirects to a local loopback listener with an
authorization code. Portico exchanges the code for a scoped token and stores
it in the existing encrypted account store. No pasting.

## Verified upstream surface (2026-08)

Checked against the live API docs:

- `POST /accounts/{account_id}/oauth_clients` creates a client. Required:
  `client_name`, `grant_types` (`authorization_code`, optionally
  `refresh_token`), `response_types` (`code`/`token`/`id_token`),
  `redirect_uris`, `scopes`. Optional: `allowed_cors_origins`,
  `allowed_scopes_optional`, `policy_uri`, `post_logout_redirect_uris`,
  visibility `public`/`private`.
- Scopes are dot-delimited (`account.read`); colon-delimited forms are
  rejected. Scope discovery exists at `GET .../oauth_scopes`.
- Client secrets support rotation (`.../rotate_secret`) so secret updates do
  not strand existing grants.
- The authorize endpoint lives under `dash.cloudflare.com/oauth2/*` and
  behaves as standard OAuth2 (unknown client → standard error redirect).
- **No device-authorization grant is offered.** A loopback redirect is the
  only viable local flow.
- Creating a client requires an account plus a token with **OAuth Client
  Write**. This is the bootstrap constraint below.

## Bootstrap constraint

Portico cannot ship a pre-created public client without someone first having
an account, so:

1. **First account:** manual token paste (current flow, now helped by the
   Phase 1 deep link).
2. **Every account after:** either reuse one configured account's token
   (with OAuth Client Write added to its permissions) to create a dedicated
   Portico OAuth client, or ask the user to create the client from a
   guided page and paste only its client ID/secret once.

After step 2, all subsequent authorizations are pure browser flow.

## Design

### Components

- `internal/provider/cloudflare/oauth.go` — client-side flow: builds the
  authorize URL (PKCE: S256 code verifier/challenge; state), runs the
  loopback listener, exchanges the code at the token endpoint, returns
  `{access_token, refresh_token, expiry}`.
- `core.SetupFlow` extension — a new declarative kind or field attribute,
  e.g. `Flow.OAuth *OAuthSpec{AuthorizeURLTemplate, ClientIDEnv, Scopes[]}`,
  so the TUI renders an "Authorize with browser" action without knowing
  Cloudflare specifics. Same principle as Phase 1's `HelpURL`: declared by
  the provider, rendered generically, never constructed by the UI.
- Loopback listener binds `127.0.0.1:<ephemeral>` with the exact port
  registered in `redirect_uris`; accepts exactly one request; validates
  `state`; closes after exchanging. Timeout ~5 minutes, cancellable from
  the TUI.
- Tokens land through the existing `ConfigureProviderAccountRequest`
  path so storage, encryption, verification, and activation are unchanged.

### Security invariants

- PKCE is mandatory even though confidential-client auth is available;
  loopback redirects are interceptable by other local processes.
- `state` is a fresh random value per attempt and is compared
  constant-time.
- The listener accepts only `127.0.0.1` connections and exactly one
  authorization callback; a second request is refused.
- Refresh tokens live only in the encrypted store (AES-GCM under the
  installation key) — same contract as every other provider secret: never in
  plans, events, logs, IPC responses, argv, or rendered views.
- The exchange happens directly against Cloudflare over HTTPS; no third
  party mediates.

### Failure handling

All failures classify through `internal/cferr` where they are Cloudflare
API errors, and render in the setup form's error slot otherwise:

- User cancels in the browser → the listener times out or receives
  `error=access_denied`; the form reports "authorization cancelled" and
  stays open.
- Browser cannot open → Phase 1's fallback message pattern: show the URL to
  paste manually.
- Expired refresh token on a later run → classify unauthorized, mark the
  account pending re-authentication, and offer the browser flow again.
  Never silently retry into a lockout loop.

### Testing strategy

Contract tests with an httptest server standing in for both the authorize
redirect and the token endpoint: happy path, state mismatch, denied consent,
malformed code response, timeout. The loopback listener gets a concurrency
test (two callbacks, second refused). Live-network behavior stays behind
the `live` build tag like the rest of Cloudflare's suite.

### Explicit non-goals

- Automating or scraping the Google/dashboard login itself: brittle,
  against ToS, and puts credentials in code paths they do not belong in.
- Device-code flow (not offered upstream).
- Multi-account fan-out from one grant: each account authorizes separately,
  matching how the account store already models identity.
