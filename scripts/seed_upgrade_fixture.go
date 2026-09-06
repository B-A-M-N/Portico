// Command seed_upgrade_fixture creates durable state used by the release
// upgrade check. It intentionally uses the normal store APIs so the fixture
// exercises the same encryption, schema, and journal formats as a real
// installation. The prior binary creates the baseline first; the current
// store then migrates and enriches that same installation for the candidate.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/B-A-M-N/portico/internal/app"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/store"
)

const (
	fixtureAccount   core.ProviderAccountID = "upgrade-fixture-account"
	fixtureOperation core.OperationID       = "upgrade-fixture-operation"
	fixturePlan      core.PlanID            = "upgrade-fixture-plan"
	fixtureResource                         = "upgrade-fixture-tunnel"
	fixtureStep                             = "upgrade-fixture-create-tunnel"
)

func fixtureConnectionID() core.ConnectionID {
	if id := os.Getenv("PORTICO_UPGRADE_FIXTURE_ID"); id != "" {
		return core.ConnectionID(id)
	}
	return "upgrade-fixture"
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "verify" {
		if err := verify(); err != nil {
			panic(err)
		}
		return
	}
	if err := seed(); err != nil {
		panic(err)
	}
}

func seed() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	paths := app.DefaultPaths()
	s, err := store.Open(paths.DatabasePath)
	if err != nil {
		return fmt.Errorf("open fixture store: %w", err)
	}
	defer s.Close()

	now := time.Now().UTC().Truncate(time.Second)
	connectionID := fixtureConnectionID()
	account := core.ProviderAccount{
		ID:            fixtureAccount,
		Provider:      "mock",
		Label:         "Upgrade fixture account",
		CredentialRef: "mock:upgrade-fixture:credential",
		Metadata:      map[string]string{"fixture": "release-upgrade"},
		Status:        core.AccountAuthenticated,
	}
	if err := s.UpsertProviderAccountCredential(ctx, account, []byte("upgrade-fixture-secret")); err != nil {
		return fmt.Errorf("seed encrypted provider credential: %w", err)
	}

	profile := &core.ConnectionProfile{
		ID:       connectionID,
		Name:     "upgrade-fixture",
		Revision: 3,
		Kind:     core.ConnectionServiceExposure,
		Spec: core.ConnectionSpec{ServiceExposure: &core.ServiceExposureSpec{
			Source: core.SourceSpec{
				Kind: core.SourceExisting,
				Existing: &core.ExistingServiceSpec{
					Network:  "tcp",
					Address:  "127.0.0.1:8080",
					Protocol: core.ProtocolHTTP,
				},
			},
			Exposure:   core.ExposureSpec{Mode: core.ExposureTemporary},
			Protection: core.ProtectionSpec{Kind: core.ProtectionNone},
		}},
		Driver: core.DriverSelection{
			DriverID:   "mock",
			ProviderID: "mock",
			AccountID:  fixtureAccount,
		},
		Lifecycle: core.LifecycleSpec{OnDisconnect: core.DisconnectKeepAlive},
		Desired:   core.DesiredOpen,
		CreatedAt: now.Add(-10 * time.Minute),
		UpdatedAt: now.Add(-2 * time.Minute),
	}
	if err := s.SaveProfile(ctx, profile); err != nil {
		return fmt.Errorf("seed profile: %w", err)
	}

	resource := &core.ProviderResource{
		ConnectionID: connectionID,
		ProviderID:   "mock",
		Type:         core.ResourceTunnel,
		ExternalID:   fixtureResource,
		Ownership:    core.OwnershipManaged,
		SpecHash:     "upgrade-fixture-spec-v1",
		Metadata:     map[string]string{"fixture": "release-upgrade", "hostname": "fixture.example.test"},
	}
	if err := s.CreateManagedResource(ctx, resource); err != nil {
		return fmt.Errorf("seed managed resource: %w", err)
	}

	runtime := &core.ConnectionRuntime{
		ConnectionID: connectionID,
		State:        core.RuntimeOpen,
		Connector: core.ConnectorRuntime{
			Status: core.ConnectorStatusStopped,
		},
		Provider: core.ProviderRuntime{
			ProviderID: "mock",
			AccountID:  fixtureAccount,
			Resources:  []core.ProviderResource{*resource},
		},
		Endpoint:       core.EndpointRuntime{PublicAddress: "https://upgrade-fixture.example.test"},
		LastObservedAt: now.Add(-90 * time.Second),
		LastTransition: now.Add(-2 * time.Minute),
	}
	if err := s.SaveRuntime(ctx, runtime); err != nil {
		return fmt.Errorf("seed runtime: %w", err)
	}

	plan := &core.OperationPlan{
		ID:              fixturePlan,
		ConnectionID:    connectionID,
		ProfileRevision: profile.Revision,
		Provider:        "mock",
		Account:         fixtureAccount,
		Intent:          core.IntentOpen,
		Steps: []core.PlanStep{{
			ID:      fixtureStep,
			Kind:    core.StepCreateTunnel,
			Summary: "Create upgrade fixture tunnel",
			Technical: core.TechnicalOperation{
				Provider: "mock",
				Type:     "mock.create_tunnel",
			},
		}},
		Expected: core.ExpectedOutcome{
			State: core.RuntimeOpen,
			Resources: []core.ResourceSummary{{
				Type:       string(core.ResourceTunnel),
				ExternalID: fixtureResource,
				Ownership:  core.OwnershipManaged,
				Lifecycle:  core.LifecyclePresent,
			}},
		},
		CreatedAt: now.Add(-90 * time.Second),
		ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := plan.ComputeFingerprint(); err != nil {
		return fmt.Errorf("fingerprint fixture plan: %w", err)
	}
	if _, err := s.SaveOrGetPlan(ctx, plan); err != nil {
		return fmt.Errorf("seed plan: %w", err)
	}
	startedAt := now.Add(-75 * time.Second).Format(time.RFC3339)
	if err := s.SaveOperation(ctx, fixtureOperation, fixturePlan, connectionID, "running", startedAt); err != nil {
		return fmt.Errorf("seed interrupted operation: %w", err)
	}
	if err := s.BeginStep(ctx, fixtureOperation, connectionID, plan.Steps[0]); err != nil {
		return fmt.Errorf("seed operation step: %w", err)
	}
	if err := s.MarkStepRecoveryRequired(ctx, fixtureOperation, fixtureStep); err != nil {
		return fmt.Errorf("seed recovery marker: %w", err)
	}

	finding := &core.DiagnosticFinding{
		ID:           "upgrade-fixture-finding",
		ConnectionID: connectionID,
		Segment:      core.SegmentConnector,
		Severity:     core.SeverityWarning,
		Summary:      "Upgrade fixture contains recoverable history",
		Explanation:  "This durable finding proves the candidate can read pre-existing diagnostics during an upgrade.",
		Evidence: []core.Evidence{{
			Type:    "fixture",
			Source:  "release-upgrade",
			Message: "seeded before the prior binary started",
		}},
		ObservedAt: now.Add(-60 * time.Second),
	}
	if err := s.SaveFinding(ctx, finding); err != nil {
		return fmt.Errorf("seed diagnostic finding: %w", err)
	}

	return nil
}

func verify() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	paths := app.DefaultPaths()
	s, err := store.Open(paths.DatabasePath)
	if err != nil {
		return fmt.Errorf("open candidate store: %w", err)
	}
	defer s.Close()

	connectionID := fixtureConnectionID()
	profile, err := s.LoadProfile(ctx, connectionID)
	if err != nil {
		return fmt.Errorf("load fixture profile: %w", err)
	}
	if profile.Name != "upgrade-fixture" || profile.Revision < 3 {
		return fmt.Errorf("fixture profile changed: name=%q revision=%d", profile.Name, profile.Revision)
	}

	accounts, err := s.ListProviderAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list fixture accounts: %w", err)
	}
	foundAccount := false
	for _, account := range accounts {
		if account.ID == fixtureAccount && account.CredentialRef == "mock:upgrade-fixture:credential" {
			foundAccount = true
			break
		}
	}
	if !foundAccount {
		return fmt.Errorf("encrypted provider account was not preserved")
	}
	secret, err := s.LoadProviderCredential(ctx, "mock", "mock:upgrade-fixture:credential")
	if err != nil {
		return fmt.Errorf("decrypt fixture credential: %w", err)
	}
	if secret != "upgrade-fixture-secret" {
		return fmt.Errorf("fixture credential changed")
	}

	resources, err := s.ListResourcesByConnection(ctx, connectionID)
	if err != nil {
		return fmt.Errorf("list fixture resources: %w", err)
	}
	foundResource := false
	for _, resource := range resources {
		if resource.ExternalID == fixtureResource && resource.Ownership == core.OwnershipManaged && resource.Lifecycle == core.LifecyclePresent {
			foundResource = true
			break
		}
	}
	if !foundResource {
		return fmt.Errorf("managed fixture resource was not preserved")
	}

	runtime, err := s.LoadRuntime(ctx, connectionID)
	if err != nil {
		return fmt.Errorf("load fixture runtime: %w", err)
	}
	if runtime.Provider.AccountID != fixtureAccount {
		return fmt.Errorf("fixture runtime account changed: %q", runtime.Provider.AccountID)
	}

	op, err := s.GetOperation(ctx, fixtureOperation)
	if err != nil {
		return fmt.Errorf("load recovered fixture operation: %w", err)
	}
	if op.State == "running" || op.State == "pending" {
		return fmt.Errorf("interrupted fixture operation was not recovered: %s", op.State)
	}
	events, err := s.GetDurableEventsForOperation(ctx, fixtureOperation)
	if err != nil {
		return fmt.Errorf("load fixture event history: %w", err)
	}
	if len(events) < 2 {
		return fmt.Errorf("fixture event history was truncated: %d events", len(events))
	}
	steps, err := s.GetStepResults(ctx, fixtureOperation)
	if err != nil {
		return fmt.Errorf("load fixture step history: %w", err)
	}
	if len(steps) == 0 {
		return fmt.Errorf("fixture recovery step history was truncated")
	}
	findings, err := s.ListFindingsByConnection(ctx, connectionID)
	if err != nil {
		return fmt.Errorf("load fixture findings: %w", err)
	}
	if len(findings) == 0 {
		return fmt.Errorf("fixture diagnostics were not preserved")
	}

	return nil
}
