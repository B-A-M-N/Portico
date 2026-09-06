package cloudflare

import (
	"context"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

func protectionSupported(caps core.Capabilities, kind core.ProtectionKind) bool {
	for _, protection := range caps.BuiltInProtection {
		if protection.Kind == kind {
			return protection.Supported
		}
	}
	return false
}

func TestAccountCapabilitiesApplyZoneReadinessWithoutChangingIntrinsicCapabilities(t *testing.T) {
	noZone := &Provider{mode: ProviderModeFull, accountID: "account-no-zone"}
	zone := &Provider{mode: ProviderModeFull, accountID: "account-zone", zoneID: "zone-1"}

	intrinsic, err := noZone.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("intrinsic Capabilities: %v", err)
	}
	if !intrinsic.CustomHostnames.Supported || !intrinsic.ManagedDNS.Supported || !protectionSupported(intrinsic, core.ProtectionEmailOTP) {
		t.Fatalf("intrinsic capabilities lost Cloudflare features: %#v", intrinsic)
	}

	noZoneCaps, err := noZone.AccountCapabilities(context.Background())
	if err != nil {
		t.Fatalf("no-zone AccountCapabilities: %v", err)
	}
	if noZoneCaps.CustomHostnames.Supported || noZoneCaps.ManagedDNS.Supported || protectionSupported(noZoneCaps, core.ProtectionEmailOTP) {
		t.Fatalf("no-zone capabilities still advertise zone operations: %#v", noZoneCaps)
	}

	zoneCaps, err := zone.AccountCapabilities(context.Background())
	if err != nil {
		t.Fatalf("zone AccountCapabilities: %v", err)
	}
	if !zoneCaps.CustomHostnames.Supported || !zoneCaps.ManagedDNS.Supported || !protectionSupported(zoneCaps, core.ProtectionEmailOTP) {
		t.Fatalf("zone capabilities lost configured operations: %#v", zoneCaps)
	}
}

func TestAccountsProviderReportsPerAccountReadinessSnapshots(t *testing.T) {
	accounts, err := NewAccountsProvider(map[core.ProviderAccountID]*Provider{
		"account-zone":    {mode: ProviderModeFull, accountID: "account-zone", zoneID: "zone-1"},
		"account-no-zone": {mode: ProviderModeFull, accountID: "account-no-zone"},
	})
	if err != nil {
		t.Fatalf("NewAccountsProvider: %v", err)
	}

	intrinsic, err := accounts.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("multi-account Capabilities: %v", err)
	}
	if !intrinsic.CustomHostnames.Supported || !intrinsic.ManagedDNS.Supported {
		t.Fatalf("multi-account intrinsic capabilities depend on account order: %#v", intrinsic)
	}

	snapshots := accounts.ReadinessSnapshots(context.Background())
	if len(snapshots) != 2 {
		t.Fatalf("readiness snapshots = %d, want 2", len(snapshots))
	}
	if snapshots[0].AccountID != "account-no-zone" || snapshots[0].ZoneConfigured {
		t.Fatalf("first snapshot = %#v, want account-no-zone without zone", snapshots[0])
	}
	if snapshots[0].Capabilities.CustomHostnames.Supported || snapshots[0].Capabilities.ManagedDNS.Supported {
		t.Fatalf("no-zone snapshot advertises zone operations: %#v", snapshots[0].Capabilities)
	}
	if snapshots[1].AccountID != "account-zone" || !snapshots[1].ZoneConfigured {
		t.Fatalf("second snapshot = %#v, want account-zone with zone", snapshots[1])
	}
	if !snapshots[1].Capabilities.CustomHostnames.Supported || !snapshots[1].Capabilities.ManagedDNS.Supported {
		t.Fatalf("zone snapshot omits configured operations: %#v", snapshots[1].Capabilities)
	}

	selected, err := accounts.ReadinessSnapshot(context.Background(), "account-zone")
	if err != nil {
		t.Fatalf("selected readiness snapshot: %v", err)
	}
	if selected.AccountID != "account-zone" || !selected.ZoneConfigured {
		t.Fatalf("selected snapshot = %#v", selected)
	}
}

func TestQuickOnlyReadinessSnapshotIsAccountIndependent(t *testing.T) {
	quick := &Provider{mode: ProviderModeQuickOnly}
	snapshot, err := quick.ReadinessSnapshot(context.Background())
	if err != nil {
		t.Fatalf("Quick Tunnel readiness: %v", err)
	}
	if !snapshot.Ready || snapshot.AccountConfigured || snapshot.ZoneConfigured {
		t.Fatalf("Quick Tunnel readiness = %#v, want ready without account or zone", snapshot)
	}
	if snapshot.Capabilities.CustomHostnames.Supported || snapshot.Capabilities.ManagedDNS.Supported {
		t.Fatalf("Quick Tunnel readiness advertises account features: %#v", snapshot.Capabilities)
	}
}
