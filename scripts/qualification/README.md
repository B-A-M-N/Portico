# Qualification scripts

Each script proves one provider's lifecycle with **independent evidence**: a
canary HTTP origin with a fixed body, an independent HTTP client, and provider
control-plane checks. Portico reporting `Status: Completed` is never treated as
proof that anything happened.

All scripts:

- are opt-in (`PORTICO_QUAL_LIVE=1`) so no routine gate touches a live provider
- run against a clean XDG environment under `artifacts/qualification/<ts>/xdg/`
- write a manifest (commit, binary SHA-256, tool versions), a command log, and
  provider-before/after evidence — never tokens
- require the canary body to be served through the real endpoint, and require
  it to become unreachable after close/delete

## Scripts

| Script | Provider | Needs |
|---|---|---|
| `local_forward.sh` | none (deterministic) | nothing external |
| `cloudflare.sh` | Cloudflare | `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, `PORTICO_CF_ZONE_NAME` |
| `tailscale.sh` | Tailscale | `PORTICO_TS_PEER` (second tailnet node with SSH) |
| `ngrok.sh` | ngrok (experimental) | `PORTICO_NGROK_LIVE=1`, `NGROK_AUTHTOKEN` |

## Run

```bash
make qualify-local            # deterministic, no accounts
PORTICO_QUAL_LIVE=1 PORTICO_CF_ZONE_NAME=qual.example.com \
CLOUDFLARE_API_TOKEN=... CLOUDFLARE_ACCOUNT_ID=... make qualify-cloudflare
PORTICO_QUAL_LIVE=1 PORTICO_TS_PEER=other-node make qualify-tailscale
PORTICO_QUAL_LIVE=1 PORTICO_NGROK_LIVE=1 NGROK_AUTHTOKEN=... make qualify-ngrok
```

## What each claims when it exits 0

- **local_forward**: create-closed → open → bytes through the listening port →
  close → socket gone → reopen → delete; unowned origin survives delete.
- **cloudflare**: quick tunnel URL serves the canary and stops on close;
  managed tunnel gets DNS verified against Cloudflare's API, unauthenticated
  requests are **denied** by Access (HTTP 302/401/403), and cleanup is verified
  provider-side.
- **tailscale**: a *second tailnet node* fetches the canary through Serve, the
  route is not publicly reachable, and the machine remains signed in after
  close (Serve config before/after compared).
- **ngrok**: the assigned endpoint carries traffic, survives a supervisor
  restart with the *same* URL (correlation, not recreation), close withdraws
  it, and the capability descriptor honestly declares no protection.

## Result files

```
artifacts/qualification/<timestamp>/
  manifest.txt      # commit, binary hash, versions, timestamps
  commands.log      # every executed command with exit status
  canary.py|.log    # the canary origin and its log
  peer_fetch.out    # tailscale: what the second node actually received
  xdg/              # the hermetic environment the run used
```

The OpenAI client tunnel remains **not live-qualified** by design: there is no
script here because readiness (`/readyz`) is not evidence of ChatGPT-side
attachment. Qualifying it requires a real OpenAI-side tunnel application and an
end-to-end tool invocation from ChatGPT.
