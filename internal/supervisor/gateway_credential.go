package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

// gateway credential provisioning (audit P0-3).
//
// The gateway's bearer token is a durable installation secret: it is
// generated once per connection, encrypted through the store's SecretStore
// (AES-GCM, context-bound AAD, re-encrypted on key rotation), and resolved to
// plaintext only transiently when the gateway is constructed. Nothing here
// logs, events, plans or serializes the plaintext.

const (
	gatewayCredentialProvider = "gateway"
	gatewayCredentialPrefix   = "gateway/"
)

// GatewayCredentialRef is the opaque credential-store reference for a
// connection's gateway token.
func GatewayCredentialRef(connID core.ConnectionID) string {
	return gatewayCredentialPrefix + string(connID)
}

// HandleRevealGatewayCredential is the narrowly scoped IPC-facing reveal
// action. The plaintext is returned only in this response and is never put in
// a snapshot or durable record.
func (h *supervisorHandler) HandleRevealGatewayCredential(connID string) (*ipc.GatewayCredentialDTO, error) {
	if h == nil || h.sup == nil {
		return nil, fmt.Errorf("supervisor is unavailable")
	}
	token, err := h.sup.RevealGatewayCredential(context.Background(), core.ConnectionID(connID))
	if err != nil {
		return nil, err
	}
	return &ipc.GatewayCredentialDTO{ConnectionID: connID, Credential: token}, nil
}

// provisionGatewayCredential returns the connection's gateway bearer tokens,
// creating and storing one only if none exists yet. The returned reference is
// safe for runtime projection; the returned tokens are plaintext and must be
// dropped once the gateway holds them.
func (s *Supervisor) provisionGatewayCredential(ctx context.Context, connID core.ConnectionID) ([]string, string, error) {
	if s.store == nil {
		return nil, "", fmt.Errorf("credential store is unavailable")
	}
	ref := GatewayCredentialRef(connID)
	existing, err := s.store.LoadProviderCredential(ctx, gatewayCredentialProvider, ref)
	if err != nil {
		return nil, "", fmt.Errorf("load gateway credential: %w", err)
	}
	if existing != "" {
		return []string{existing}, ref, nil
	}

	token, err := generateGatewayToken()
	if err != nil {
		return nil, "", fmt.Errorf("generate gateway credential: %w", err)
	}
	if err := s.store.SaveProviderCredential(ctx, gatewayCredentialProvider, ref, []byte(token)); err != nil {
		return nil, "", fmt.Errorf("store gateway credential: %w", err)
	}
	return []string{token}, ref, nil
}

// generateGatewayToken produces 256 bits of random material, hex-encoded.
// No fragment of the value is ever logged: even a prefix narrows an
// attacker's search space.
func generateGatewayToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	// Best-effort scrub of the intermediate buffer; the hex string itself is
	// zeroed by the caller when practical.
	for i := range buf {
		buf[i] = 0
	}
	return token, nil
}

// RevealGatewayCredential resolves the connection's gateway bearer token for
// the narrowly scoped user-facing reveal action. This is the ONE sanctioned
// path from the durable credential to a human; callers must not log, event,
// export or persist what it returns.
func (s *Supervisor) RevealGatewayCredential(ctx context.Context, connID core.ConnectionID) (string, error) {
	ref := GatewayCredentialRef(connID)
	token, err := s.store.LoadProviderCredential(ctx, gatewayCredentialProvider, ref)
	if err != nil {
		return "", fmt.Errorf("resolve gateway credential: %w", err)
	}
	if token == "" {
		return "", fmt.Errorf("this connection has no gateway credential yet; open it once to provision one")
	}
	return token, nil
}
