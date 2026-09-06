package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/B-A-M-N/portico/internal/core"
)

// CredentialHealth is read-only status metadata about one encrypted
// credential record. It never carries plaintext: doctor prints these and
// nothing else.
type CredentialHealth struct {
	// Kind distinguishes the credential table the record lives in.
	Kind string // "provider_credential" | "tunnel_credentials" | "provider_account"
	// Reference is the credential's opaque address (credential_ref,
	// connection ID, or account ID). Never a secret.
	Reference string
	// KeyVersion is the key version the record was encrypted under, when
	// its blob declares one. Legacy raw blobs report 0.
	KeyVersion int
	// Problem describes why the record cannot be decrypted; empty means
	// healthy. Values are classification strings plus detail, suitable for
	// display — never secret material.
	Problem string
}

// CredentialHealthReport summarises every encrypted credential record.
type CredentialHealthReport struct {
	Total     int
	Healthy   int
	Unhealthy int
	Records   []CredentialHealth
}

// CheckCredentialHealth enumerates every encrypted credential record in the
// database WITHOUT exposing plaintext, validates that the key version each
// blob references exists, and attempts an authenticated decryption of each
// record internally so a wrong key, a missing historical key version, corrupt
// ciphertext or a context mismatch is detected here rather than the first
// time a connection needs the credential.
//
// Read-only by contract: no row is rewritten, no key is rotated, nothing is
// deleted. The supervisor stays the sole DB opener — this API exists on the
// store the supervisor already owns and is reached through it, not by the CLI
// opening SQLite itself.
func (s *Store) CheckCredentialHealth(ctx context.Context) (*CredentialHealthReport, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	report := &CredentialHealthReport{}

	// provider_credentials rows (migration 16+).
	rows, err := s.db.QueryContext(ctx,
		`SELECT credential_ref, provider_id, secret_encrypted FROM provider_credentials`)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("enumerate provider credentials: %w", err)
	}
	if err == nil {
		for rows.Next() {
			var ref, providerID string
			var encrypted []byte
			if err := rows.Scan(&ref, &providerID, &encrypted); err != nil {
				rows.Close()
				return nil, err
			}
			record := CredentialHealth{Kind: "provider_credential", Reference: ref}
			record.KeyVersion = declaredKeyVersion(encrypted)
			if _, decErr := decryptCredential(s.secretStore, encrypted, providerCredentialContext(core.ProviderID(providerID), ref)); decErr != nil {
				record.Problem = classifyCredentialProblem(decErr)
			}
			report.add(record)
		}
		rows.Close()
	}

	// tunnel_credentials rows (migration 9+). Context binds connection,
	// provider, tunnel and schema version; legacy machine-ID blobs are
	// readable through decryptTunnelCredentialLocked's migration path.
	tRows, err := s.db.QueryContext(ctx,
		`SELECT connection_id, COALESCE(provider_id, ''), tunnel_id, token_encrypted FROM tunnel_credentials`)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("enumerate tunnel credentials: %w", err)
	}
	if err == nil {
		for tRows.Next() {
			var connID, providerID, tunnelID string
			var encrypted []byte
			if err := tRows.Scan(&connID, &providerID, &tunnelID, &encrypted); err != nil {
				tRows.Close()
				return nil, err
			}
			record := CredentialHealth{
				Kind:      "tunnel_credentials",
				Reference: string(core.ConnectionID(connID)),
			}
			record.KeyVersion = declaredKeyVersion(encrypted)
			if _, _, decErr := s.decryptTunnelCredentialLocked(
				core.ConnectionID(connID), core.ProviderID(providerID), tunnelID, encrypted); decErr != nil {
				record.Problem = classifyCredentialProblem(decErr)
			}
			report.add(record)
		}
		tRows.Close()
	}

	return report, nil
}

func (r *CredentialHealthReport) add(rec CredentialHealth) {
	r.Total++
	if rec.Problem == "" {
		r.Healthy++
	} else {
		r.Unhealthy++
	}
	r.Records = append(r.Records, rec)
}

// declaredKeyVersion reads the key version an EncryptedBlob JSON payload
// declares without decrypting anything. A non-JSON blob (legacy raw AES-GCM
// bytes) reports 0.
func declaredKeyVersion(encrypted []byte) int {
	var blob EncryptedBlob
	if json.Unmarshal(encrypted, &blob) != nil {
		return 0
	}
	return blob.KeyVersion
}

// classifyCredentialProblem reduces a decryption failure to one of the four
// causes that matter operationally. The distinction matters because the
// remedies differ: wrong/missing key means restore or re-setup; corruption or
// context mismatch means re-save the credential.
func classifyCredentialProblem(err error) string {
	msg := err.Error()
	switch {
	case containsAny(msg, "unsupported key version"):
		return "missing historical key version"
	case containsAny(msg, "context mismatch"):
		return "context mismatch"
	case containsAny(msg, "chacha20poly1305: message authentication failed",
		"cipher: message authentication failed", "decrypt:", "ciphertext too short",
		"unmarshal blob", "malformed"):
		return "corrupt ciphertext"
	default:
		return "decryption failed"
	}
}

func containsAny(s string, substrings ...string) bool {
	for _, sub := range substrings {
		if len(sub) > 0 && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
