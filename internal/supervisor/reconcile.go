package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/controller"
	"github.com/B-A-M-N/portico/internal/core"
)

// ReconcileInput contains all the information needed to compute
// the smallest plan to reach desired state.
type ReconcileInput struct {
	Profile   *core.ConnectionProfile
	Runtime   *core.ConnectionRuntime
	Resources []core.ProviderResource
	Observed  *core.ObservedConnection
}

// reconcileDecision represents the action needed for a connection.
type reconcileDecision struct {
	Action string // "none", "open", "close", "repair", "recreate"
	Plan   *core.OperationPlan
	// Blocked reports that the desired state cannot be realised at all, so
	// reconciliation has stopped rather than found nothing to do. The two are
	// very different to an operator and must not both read as "none".
	Blocked error
}

// computeReconcileDecision compares desired vs observed state and
// returns the smallest action needed. This is the single source of
// truth for restart, reconciliation, and repair planning.
func (s *Supervisor) computeReconcileDecision(ctx context.Context, input ReconcileInput) (*reconcileDecision, error) {
	desired := input.Profile.Desired

	// Reconciliation is kind-specific. The current implementation handles
	// service-exposure (Cloudflare) connections. Other kinds have their own
	// reconciliation semantics that are not yet implemented.
	if input.Profile.Kind != core.ConnectionServiceExposure {
		return &reconcileDecision{Action: "none"}, nil
	}

	// Reconciliation moves a connection toward its desired state without
	// anyone asking, so it must not drive a connection toward a state that is
	// no longer permitted. A profile stored under an older rule would
	// otherwise have its connector restarted or its provider resources
	// recreated on a loop, re-establishing exactly what the rule forbids, with
	// nothing reporting why.
	//
	// Only the open direction is gated: a connection that can no longer open
	// must still be closable.
	if desired == core.DesiredOpen {
		if err := input.Profile.ValidateForOpen(); err != nil {
			// Reported rather than logged: this function stays a pure decision,
			// and the caller records a durable finding. "Nothing to do" and
			// "cannot proceed" would otherwise be indistinguishable.
			return &reconcileDecision{Action: "none", Blocked: err}, nil
		}
	}

	// A missing runtime projection does not imply missing infrastructure. An
	// interrupted bootstrap can leave a durable tunnel inventory with no
	// runtime row; retain it and start only the connector when observation did
	// not authoritatively report it missing.
	if input.Runtime == nil {
		if desired == core.DesiredOpen {
			// A missing runtime projection must not suppress a narrower
			// resource repair. Durable inventory plus authoritative observation
			// are sufficient to recreate a missing DNS or Access component
			// without first starting a connector or rebuilding a tunnel.
			if plan := resourceDeltaRepairPlan(input); plan != nil {
				return &reconcileDecision{Action: "repair", Plan: plan}, nil
			}
			if tunnel := liveTunnelResource(input.Resources); tunnel != nil && !observedMissing(input.Observed, core.ResourceTunnel, tunnel.ExternalID) {
				if plan := s.buildConnectorRestartPlan(input.Profile.ID, input.Profile); plan != nil {
					return &reconcileDecision{Action: "repair", Plan: plan}, nil
				}
			}
			plan, err := s.controller.PlanOpen(ctx, input.Profile.ID)
			if err != nil {
				return nil, err
			}
			return &reconcileDecision{Action: "open", Plan: plan}, nil
		}
		return &reconcileDecision{Action: "none"}, nil
	}

	// Desired closed dominates - do not repair a connection the user wants closed.
	if desired == core.DesiredClosed {
		if input.Runtime.State == core.RuntimeClosed {
			return &reconcileDecision{Action: "none"}, nil
		}
		plan, err := s.controller.PlanClose(ctx, input.Profile.ID)
		if err != nil {
			return nil, err
		}
		return &reconcileDecision{Action: "close", Plan: plan}, nil
	}

	// Desired open - check what needs to be done.
	switch input.Runtime.State {
	case core.RuntimeOpen:
		if plan := resourceDeltaRepairPlan(input); plan != nil {
			return &reconcileDecision{Action: "repair", Plan: plan}, nil
		}
		// Owned origin health is part of the open chain. A dead owned origin
		// must be repaired before reporting the connection as healthy.
		if input.Runtime.Origin.Ownership == core.OriginOwnershipOwned &&
			input.Runtime.Origin.Status != core.OriginStatusRunning {
			return s.repairDecision(ctx, input.Profile.ID)
		}
		// Check connector health.
		if input.Runtime.Connector.Status == core.ConnectorStatusRunning {
			return &reconcileDecision{Action: "none"}, nil
		}
		if input.Runtime.Connector.Status == core.ConnectorStatusUnknown {
			// Identity could not be verified: never signal the PID —
			// require an explicit repair instead of a blind restart.
			return s.repairDecision(ctx, input.Profile.ID)
		}
		// Connector not running - restart it.
		plan := s.buildConnectorRestartPlan(input.Profile.ID, input.Profile)
		if plan == nil {
			return &reconcileDecision{Action: "none"}, nil
		}
		return &reconcileDecision{Action: "repair", Plan: plan}, nil

	case core.RuntimeClosed, core.RuntimeUnknown:
		// Delta planning: if a live tunnel resource is tracked and the
		// provider has not authoritatively confirmed it missing, retain
		// existing infrastructure and only restart the connector.
		// Full recreation requires either no live tunnel or a confirmed
		// not-found from observation.
		tunnel := liveTunnelResource(input.Resources)
		if tunnel != nil && !observedMissing(input.Observed, core.ResourceTunnel, tunnel.ExternalID) {
			plan := s.buildConnectorRestartPlan(input.Profile.ID, input.Profile)
			if plan != nil {
				return &reconcileDecision{Action: "repair", Plan: plan}, nil
			}
		}
		// Need full open plan.
		plan, err := s.controller.PlanOpen(ctx, input.Profile.ID)
		if err != nil {
			return nil, err
		}
		return &reconcileDecision{Action: "open", Plan: plan}, nil

	case core.RuntimeDegraded, core.RuntimeError:
		// Resource drift is independent of the runtime projection. A previous
		// connector or provider error must not hide an exact authoritative
		// repair opportunity (for example a missing Access policy).
		if plan := resourceDeltaRepairPlan(input); plan != nil {
			return &reconcileDecision{Action: "repair", Plan: plan}, nil
		}
		return s.repairDecision(ctx, input.Profile.ID)

	case core.RuntimeOrphaned:
		// Managed resources need cleanup.
		plan, err := s.controller.PlanDelete(ctx, input.Profile.ID)
		if err != nil {
			return nil, err
		}
		return &reconcileDecision{Action: "recreate", Plan: plan}, nil
	}

	return &reconcileDecision{Action: "none"}, nil
}

// resourceDeltaRepairPlan creates the smallest exact-ID repair for an
// authoritative desired-versus-observed resource delta. It intentionally
// handles one causal delta per operation; the next reconcile pass evaluates
// the resulting state rather than combining unrelated remote mutations.
func resourceDeltaRepairPlan(input ReconcileInput) *core.OperationPlan {
	profile := input.Profile
	// Access resources are independent from the tunnel and connector. A
	// protected profile whose exact Access application was confirmed absent
	// needs a narrow application-and-policy recreation; do not recreate the
	// tunnel or touch DNS. If only the policy is absent, retain the exact
	// observed application and restore only that policy.
	if profile.IsProtected() {
		if app := missingTrackedResource(input, core.ResourceAccessApp); app != nil {
			var oldPolicyID string
			if policy := missingTrackedResource(input, core.ResourceAccessPolicy); policy != nil {
				oldPolicyID = policy.ExternalID
			}
			return buildAccessAppCreatePlan(profile, app.ExternalID, oldPolicyID)
		}
		if policy := missingTrackedResource(input, core.ResourceAccessPolicy); policy != nil {
			if app := presentTrackedResource(input, core.ResourceAccessApp); app != nil {
				return buildAccessPolicyCreatePlan(profile, app.ExternalID, policy.ExternalID)
			}
		}
		if app := driftedAccessAppResource(input); app != nil && profile.GetExposure().RequestedAddress != "" {
			return buildAccessAppUpdatePlan(profile, app.ExternalID)
		}
		if policy, appID := driftedAccessPolicyResource(input); policy != nil {
			return buildAccessPolicyUpdatePlan(profile, policy.ExternalID, appID)
		}
	}
	// A present DNS record can still point at the wrong tunnel. Compare the
	// authoritative exact-record observation to the durable tunnel ID and
	// update only that record when it has drifted.
	if dns, tunnel := driftedDNSResource(input); dns != nil && tunnel != nil && profile.GetExposure().RequestedAddress != "" {
		return buildDNSUpdatePlan(profile, dns.ExternalID, tunnel.ExternalID)
	}
	// An authoritative missing DNS record with an intact tunnel is a narrow
	// repair. Transient, unauthorized, and rate-limited observations never
	// enter missingTrackedResource.
	if dns := missingTrackedResource(input, core.ResourceDNSRecord); dns != nil && liveTunnelResource(input.Resources) != nil && profile.GetExposure().RequestedAddress != "" {
		return buildDNSCreatePlan(profile, dns.ExternalID, liveTunnelResource(input.Resources).ExternalID)
	}
	return nil
}

func missingTrackedResource(input ReconcileInput, resourceType core.ResourceType) *core.ProviderResource {
	for i := range input.Resources {
		resource := &input.Resources[i]
		if resource.Type == resourceType && resourceIsLive(*resource) && observedMissing(input.Observed, resourceType, resource.ExternalID) {
			return resource
		}
	}
	return nil
}

func presentTrackedResource(input ReconcileInput, resourceType core.ResourceType) *core.ProviderResource {
	for i := range input.Resources {
		resource := &input.Resources[i]
		if resource.Type == resourceType && resourceIsLive(*resource) && observedPresent(input.Observed, resourceType, resource.ExternalID) {
			return resource
		}
	}
	return nil
}

func resourceIsLive(resource core.ProviderResource) bool {
	switch resource.Lifecycle {
	case core.LifecycleRemoved, core.LifecycleExternallyRemoved:
		return false
	default:
		return true
	}
}

// driftedDNSResource returns a DNS record only when its exact remote
// observation is present and its target differs from the tracked tunnel.
// Missing, unauthorized, rate-limited, and transient observations are never
// interpreted as drift.
func driftedDNSResource(input ReconcileInput) (*core.ProviderResource, *core.ProviderResource) {
	tunnel := liveTunnelResource(input.Resources)
	if tunnel == nil || input.Observed == nil {
		return nil, nil
	}
	wantTarget := normalizeDNSTarget(tunnel.ExternalID + ".cfargotunnel.com")
	for i := range input.Resources {
		resource := &input.Resources[i]
		if resource.Type != core.ResourceDNSRecord || !resourceIsLive(*resource) || !observedPresent(input.Observed, resource.Type, resource.ExternalID) {
			continue
		}
		for _, record := range input.Observed.DNSRecords {
			if record.ID == resource.ExternalID && record.Target != "" && normalizeDNSTarget(record.Target) != wantTarget {
				return resource, tunnel
			}
		}
	}
	return nil, nil
}

// driftedAccessAppResource returns an exact present Access application whose
// configured domain no longer matches the desired public hostname. Observation
// uncertainty and untracked applications are never interpreted as drift.
func driftedAccessAppResource(input ReconcileInput) *core.ProviderResource {
	if input.Observed == nil {
		return nil
	}
	wantDomain := normalizeAccessDomain(input.Profile.GetExposure().RequestedAddress)
	if wantDomain == "" {
		return nil
	}
	for i := range input.Resources {
		resource := &input.Resources[i]
		if resource.Type != core.ResourceAccessApp || !resource.IsLive() || !observedPresent(input.Observed, resource.Type, resource.ExternalID) {
			continue
		}
		for _, app := range input.Observed.AccessApps {
			if app.ID == resource.ExternalID && normalizeAccessDomain(app.Domain) != wantDomain {
				return resource
			}
		}
	}
	return nil
}

func normalizeAccessDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

// driftedAccessPolicyResource returns an exact present policy whose allow
// identities, session duration, or decision differs from the desired
// protection. Only fields Portico owns are compared; unknown provider policy
// features neither trigger mutation nor get overwritten.
func driftedAccessPolicyResource(input ReconcileInput) (*core.ProviderResource, string) {
	if input.Observed == nil {
		return nil, ""
	}
	for i := range input.Resources {
		resource := &input.Resources[i]
		if resource.Type != core.ResourceAccessPolicy || !resource.IsLive() || !observedPresent(input.Observed, resource.Type, resource.ExternalID) {
			continue
		}
		appID := resource.Metadata["app_id"]
		if appID == "" || !observedPresent(input.Observed, core.ResourceAccessApp, appID) {
			continue
		}
		for _, policy := range input.Observed.AccessPolicies {
			if policy.ID != resource.ExternalID || policy.AppID != appID {
				continue
			}
			if accessPolicyDrifted(input.Profile.GetProtection(), policy) {
				return resource, appID
			}
		}
	}
	return nil, ""
}

func accessPolicyDrifted(want core.ProtectionSpec, observed core.ObservedAccessPolicy) bool {
	if !strings.EqualFold(observed.Decision, "allow") || !sameNormalizedStrings(want.AllowedEmails, observed.AllowedEmails, false) || !sameNormalizedStrings(want.AllowedDomains, observed.AllowedDomains, true) {
		return true
	}
	// A zero TTL means the profile did not request a policy-specific session
	// duration; retain the provider default instead of manufacturing drift.
	if want.SessionTTL > 0 && strings.TrimSpace(observed.SessionDuration) != want.SessionTTL.String() {
		return true
	}
	return false
}

func sameNormalizedStrings(a, b []string, lowercase bool) bool {
	normalize := func(values []string) []string {
		out := make([]string, 0, len(values))
		for _, value := range values {
			value = strings.TrimSpace(value)
			if lowercase {
				value = strings.ToLower(value)
			}
			if value != "" {
				out = append(out, value)
			}
		}
		sort.Strings(out)
		return out
	}
	a, b = normalize(a), normalize(b)
	return slicesEqual(a, b)
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func normalizeDNSTarget(target string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(target)), ".")
}

// buildDNSCreatePlan materializes the exact tracked tunnel ID into the repair
// plan. The provider adapter cannot rely on in-memory connection state after
// a supervisor restart; the durable inventory is the authority for this
// narrowly scoped DNS repair.
func buildDNSCreatePlan(profile *core.ConnectionProfile, previousRecordID, tunnelID string) *core.OperationPlan {
	now := time.Now().UTC()
	plan := &core.OperationPlan{
		ID: core.NewPlanID(), ConnectionID: profile.ID, ProfileRevision: profile.Revision,
		Provider: profile.GetProvider().ProviderID, Intent: core.IntentRepair, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		Steps: []core.PlanStep{{
			ID: "repair-dns-" + previousRecordID, Kind: core.StepCreateDNSRecord,
			Summary: fmt.Sprintf("Recreate DNS CNAME for %s", profile.GetExposure().RequestedAddress),
			Technical: core.TechnicalOperation{Provider: profile.GetProvider().ProviderID, Type: "create_dns", Parameters: map[string]string{
				"hostname":  profile.GetExposure().RequestedAddress,
				"tunnel_id": tunnelID,
			}},
		}},
	}
	_ = plan.ComputeFingerprint()
	return plan
}

func buildDNSUpdatePlan(profile *core.ConnectionProfile, recordID, tunnelID string) *core.OperationPlan {
	now := time.Now().UTC()
	plan := &core.OperationPlan{
		ID: core.NewPlanID(), ConnectionID: profile.ID, ProfileRevision: profile.Revision,
		Provider: profile.GetProvider().ProviderID, Intent: core.IntentRepair, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		Steps: []core.PlanStep{{
			ID: "repair-dns-target-" + recordID, Kind: core.StepUpdateDNSRecord,
			Summary: "Correct DNS CNAME target for " + profile.GetExposure().RequestedAddress,
			Technical: core.TechnicalOperation{Provider: profile.GetProvider().ProviderID, Type: "update_dns", ResourceID: recordID, Parameters: map[string]string{
				"hostname":  profile.GetExposure().RequestedAddress,
				"tunnel_id": tunnelID,
			}},
		}},
	}
	_ = plan.ComputeFingerprint()
	return plan
}

func accessRepairParameters(profile *core.ConnectionProfile) map[string]string {
	prot := profile.GetProtection()
	return map[string]string{
		"hostname":         profile.GetExposure().RequestedAddress,
		"protection_kind":  string(prot.Kind),
		"allowed_emails":   strings.Join(prot.AllowedEmails, ","),
		"allowed_domains":  strings.Join(prot.AllowedDomains, ","),
		"session_duration": prot.SessionTTL.String(),
	}
}

// buildAccessAppCreatePlan restores an Access application and its initial
// policy only after the old application was authoritatively reported missing.
// The replacement IDs are marked externally removed in the same successful
// step transaction, preventing repeat repairs against a known-dead resource.
func buildAccessAppCreatePlan(profile *core.ConnectionProfile, oldAppID, oldPolicyID string) *core.OperationPlan {
	params := accessRepairParameters(profile)
	params["replaces_access_app_id"] = oldAppID
	if oldPolicyID != "" {
		params["replaces_access_policy_id"] = oldPolicyID
	}
	return buildAccessRepairPlan(profile, "repair-access-app-"+oldAppID, core.StepCreateAccessApp,
		"Recreate Access application and policy", "create_access_app", "", params)
}

// buildAccessPolicyCreatePlan restores only an exact missing policy beneath
// an exact present application. It never creates another application.
func buildAccessPolicyCreatePlan(profile *core.ConnectionProfile, appID, oldPolicyID string) *core.OperationPlan {
	params := accessRepairParameters(profile)
	params["app_id"] = appID
	params["replaces_access_policy_id"] = oldPolicyID
	return buildAccessRepairPlan(profile, "repair-access-policy-"+oldPolicyID, core.StepCreateAccessPolicy,
		"Recreate Access policy", "create_access_policy", oldPolicyID, params)
}

func buildAccessAppUpdatePlan(profile *core.ConnectionProfile, appID string) *core.OperationPlan {
	return buildAccessRepairPlan(profile, "repair-access-app-domain-"+appID, core.StepUpdateAccessApp,
		"Correct Access application hostname", "update_access_app", appID, map[string]string{
			"hostname": profile.GetExposure().RequestedAddress,
		})
}

func buildAccessPolicyUpdatePlan(profile *core.ConnectionProfile, policyID, appID string) *core.OperationPlan {
	params := accessRepairParameters(profile)
	params["app_id"] = appID
	return buildAccessRepairPlan(profile, "repair-access-policy-spec-"+policyID, core.StepUpdateAccessPolicy,
		"Correct Access policy", "update_access_policy", policyID, params)
}

func buildAccessRepairPlan(profile *core.ConnectionProfile, stepID string, kind core.StepKind, summary, operationType, resourceID string, params map[string]string) *core.OperationPlan {
	now := time.Now().UTC()
	plan := &core.OperationPlan{
		ID: core.NewPlanID(), ConnectionID: profile.ID, ProfileRevision: profile.Revision,
		Provider: profile.GetProvider().ProviderID, Intent: core.IntentRepair, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		Steps: []core.PlanStep{{
			ID: stepID, Kind: kind, Summary: summary,
			Technical: core.TechnicalOperation{Provider: profile.GetProvider().ProviderID, Type: operationType, ResourceID: resourceID, Parameters: params},
		}},
	}
	_ = plan.ComputeFingerprint()
	return plan
}

// repairDecision plans a repair, mapping the typed no-repair result to "none".
func (s *Supervisor) repairDecision(ctx context.Context, connID core.ConnectionID) (*reconcileDecision, error) {
	plan, err := s.controller.PlanRepair(ctx, connID)
	if errors.Is(err, controller.ErrNoRepairNeeded) {
		return &reconcileDecision{Action: "none"}, nil
	}
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return &reconcileDecision{Action: "none"}, nil
	}
	return &reconcileDecision{Action: "repair", Plan: plan}, nil
}

// liveTunnelResource returns the tracked tunnel resource that is still
// considered live (not removed), or nil.
func liveTunnelResource(resources []core.ProviderResource) *core.ProviderResource {
	for i := range resources {
		r := &resources[i]
		if r.Type != core.ResourceTunnel {
			continue
		}
		if !resourceIsLive(*r) {
			continue
		}
		return r
	}
	return nil
}

// observedMissing reports whether observation authoritatively classified
// the given resource as not found. Transient, unauthorized, or absent
// observations never count as missing.
func observedMissing(obs *core.ObservedConnection, resType core.ResourceType, externalID string) bool {
	if obs == nil {
		return false
	}
	for _, st := range obs.ResourceStatuses {
		if st.Type == resType && st.ExternalID == externalID {
			return st.Status == core.ObservationMissing
		}
	}
	return false
}

func observedPresent(obs *core.ObservedConnection, resType core.ResourceType, externalID string) bool {
	if obs == nil {
		return false
	}
	for _, st := range obs.ResourceStatuses {
		if st.Type == resType && st.ExternalID == externalID {
			return st.Status == core.ObservationPresent
		}
	}
	return false
}
