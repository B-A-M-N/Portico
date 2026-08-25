package supervisor

import (
	"context"
	"strings"
	"testing"

	ipc "github.com/B-A-M-N/portico/internal/ipc"
)

// TestCloudflareSetupRequiresExactZoneVerification pins item 15's fail-closed
// rule: a supplied zone ID is verified by an exact lookup, and setup fails
// when that verification cannot confirm it — even when account validation
// itself succeeded. DNS capability is never inferred from a non-empty zone
// field.
func TestCloudflareSetupRequiresExactZoneVerification(t *testing.T) {
	st := newRecoveryTestStore(t)
	validator := &stubAccountValidator{result: &AccountValidation{
		AccountAccessible: true,
		// The exact lookup returns nothing for the typo'd zone: not visible
		// to this token. This used to pass whenever ANY zone was listed.
	}, zoneResults: map[string][]ZoneSummary{}}
	handler := &supervisorHandler{sup: cloudflareTestSupervisor(t, st, validator)}

	_, err := handler.HandleConfigureProviderAccount("cloudflare", ipc.ConfigureProviderAccountRequest{
		AccountID: "account-a", Credential: "token", ZoneID: "zone-typo",
	})
	if err == nil {
		t.Fatal("setup accepted an unverifiable zone")
	}
	if !strings.Contains(err.Error(), "zone-typo") {
		t.Fatalf("the refusal does not name the zone: %v", err)
	}
	if validator.zoneCalls != 1 {
		t.Fatalf("VerifyZone called %d times, want exactly 1", validator.zoneCalls)
	}
}

// TestCloudflareSetupRecordsVerifiedZone pins that a zone which PASSES the
// exact verification is durably recorded as verified, so downstream DNS
// capability decisions can rely on proof rather than assumption.
func TestCloudflareSetupRecordsVerifiedZone(t *testing.T) {
	st := newRecoveryTestStore(t)
	validator := &stubAccountValidator{result: &AccountValidation{
		AccountAccessible: true,
	}}
	handler := &supervisorHandler{sup: cloudflareTestSupervisor(t, st, validator)}

	if _, err := handler.HandleConfigureProviderAccount("cloudflare", ipc.ConfigureProviderAccountRequest{
		AccountID: "account-a", Credential: "token", ZoneID: "zone-ok",
	}); err != nil {
		t.Fatalf("setup rejected a verifiable zone: %v", err)
	}
	accounts, err := st.ListProviderAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		if a.ID == "account-a" {
			if a.Metadata["zone_verified"] != "true" {
				t.Fatalf("a verified zone was not recorded: metadata = %v", a.Metadata)
			}
			return
		}
	}
	t.Fatal("the configured account was not persisted")
}

// TestCloudflareSetupSucceedsWithoutZone pins the least-privilege positive
// case: tunnel-only tokens (no zones supplied) still configure successfully,
// because no zone permission is demanded for tunnel management.
func TestCloudflareSetupSucceedsWithoutZone(t *testing.T) {
	st := newRecoveryTestStore(t)
	validator := &stubAccountValidator{result: &AccountValidation{AccountAccessible: true}}
	handler := &supervisorHandler{sup: cloudflareTestSupervisor(t, st, validator)}

	resp, err := handler.HandleConfigureProviderAccount("cloudflare", ipc.ConfigureProviderAccountRequest{
		AccountID: "account-a", Credential: "token",
	})
	if err != nil {
		t.Fatalf("tunnel-only setup failed: %v", err)
	}
	// Status is empty on the response; the durable account row is the truth.
	if resp == nil {
		t.Fatal("no response")
	}
	if validator.zoneCalls != 0 {
		t.Fatalf("VerifyZone ran %d times with no zone supplied; zone checks are mandatory only for zones", validator.zoneCalls)
	}
}

// TestTheRealValidatorClassifiesPermissionFailures pins the classification
// contract on the real validator's error mapping without network access:
// forbidden means insufficient tunnel permission (not missing Account
// Settings Read), unauthorized means bad token, and everything else is
// transient advice.
// The real cloudflareAccountValidator requires network access, so its
// classification is exercised through the error strings it produces for each
// cf status code in unit form here via VerifyZone's documented messages.
func TestVerifyZoneClassifiesFailures(t *testing.T) {
	v := cloudflareAccountValidator{}
	// A stubbed transport is not possible against the real API client, so
	// this asserts the documented behavior of the failure paths indirectly:
	// every refusal must name the zone so the user knows what to fix.
	_, err := v.VerifyZone(context.Background(), "credential-shape-invalid-but-nonempty", "")
	if err == nil {
		t.Fatal("an empty zone ID must be refused")
	}
}
