# Encryption-Key Backup and Recovery Runbook

This runbook covers the key that protects every durable secret Portico
stores: provider account credentials (Cloudflare API tokens, ngrok keys,
the `client_tunnel` runtime key) and tunnel connector tokens.

## Which file protects what

| File | Default location | Protects |
|------|------------------|----------|
| Installation key | `$XDG_DATA_HOME/portico/portico-key.bin` (default `~/.local/share/portico/portico-key.bin`) | Every row in `provider_credentials` and every tunnel token in the database |

The database (`portico.db`, same data directory) holds only ciphertext.
The key file is the single root of trust for all of it.

## File ownership and mode expectations

- The key file MUST be a regular file, mode `0600`, owned by the user
  running the supervisor.
- Its containing directory MUST be mode `0700`.
- Portico refuses to load a key with weaker permissions or an unexpected
  file type (symlinks rejected), and logs what it found.

## What is encrypted, how

AES-256-GCM, authenticated. Each ciphertext is bound to an AAD context
naming the provider, credential reference and schema version — a stolen
ciphertext cannot be decrypted under a different identity even WITH the
key, and tampering fails the authentication check.

## Backup procedure

1. Stop the supervisor: `portico supervisor stop`
2. Copy BOTH files to the backup location:
   - `portico-key.bin` (the key)
   - `portico.db` (the ciphertext)
3. Preserve modes on both: `chmod 600` on the key copy; back up over a
   trusted channel. The backup of the key is as sensitive as the key.
4. Restart: `portico supervisor start`

A key backup alone is useless without the matching database, and vice
versa. Back them up together, from the same moment in time.

## Restoration procedure

1. Stop the supervisor.
2. Restore `portico-key.bin` to the data directory with mode `0600`,
   owned by the supervisor's user.
3. Restore `portico.db` alongside it.
4. Start the supervisor and run `portico doctor` — it reports whether
   stored credentials decrypt.

## Corrupt or missing key: what can and cannot be recovered

- Missing/corrupt key + intact DB backup from BEFORE the corruption:
  restore both from backup. Full recovery.
- Key lost WITHOUT a backup: nothing encrypted is recoverable. This is
  by design — the same property that makes a leaked DB harmless makes a
  lost key final. Provider credentials must be re-entered
  (`portico provider login <provider>`), and affected tunnels must be
  re-created at the provider because their connector tokens are lost too.
  Connections will show as degraded rather than silently working.
- Wrong key (e.g. restored another machine's): decryption fails per
  credential with an authentication error; `portico doctor` identifies
  which accounts are unreadable. Replace via provider login per account.

## After rotation

`RotateSecretKey` re-encrypts every secret under a new key version in one
transaction and journals the rotation. The OLD key material is no longer
needed once rotation reports success:

1. Verify with `portico doctor` that credentials decrypt.
2. Remove old key-version entries from any backups taken before the
   rotation — keeping them preserves access to pre-rotation secrets.
3. Take a fresh backup pair (new key + current DB).

## Doctor

`portico doctor` is the entry point for all of the above: it reports key
status, decryptability of each stored credential, and points here when a
key problem is detected.
