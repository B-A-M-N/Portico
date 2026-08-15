package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
)

// ProfileDelta describes what an edit changes and what it invalidates.
type ProfileDelta struct {
	// Changes are human-readable descriptions of what differs.
	Changes []string
	// RestartConnector reports that the running connector no longer matches the
	// profile and must be restarted even if no provider resource changes.
	RestartConnector bool
	// InvalidatedResources are the resource types the proposed profile no
	// longer describes. They are deleted during the edit and recreated by the
	// reopen.
	InvalidatedResources map[core.ResourceType]bool
}

// Empty reports an edit that changes nothing.
func (d ProfileDelta) Empty() bool {
	return len(d.Changes) == 0
}

// NameOnly reports an edit that touches nothing at the provider and needs no
// plan, so renaming does not become a preview-and-confirm workflow.
func (d ProfileDelta) NameOnly() bool {
	return len(d.Changes) == 1 && d.Changes[0] == "name" && !d.RestartConnector
}

// invalidate marks a resource type for replacement.
func (d *ProfileDelta) invalidate(kinds ...core.ResourceType) {
	if d.InvalidatedResources == nil {
		d.InvalidatedResources = map[core.ResourceType]bool{}
	}
	for _, k := range kinds {
		d.InvalidatedResources[k] = true
	}
}

// DiffProfiles classifies an edit.
//
// The classification is derived from the fields that changed rather than
// inferred at execution time, so the plan preview can state exactly which
// provider resources will be replaced before the user confirms.
//
// Currently supports service-exposure connections fully. For other kinds
// (port_forward, private_network, client_tunnel), generic metadata changes
// (name, lifecycle) are detected, but spec-specific changes are not yet
// classified. This is intentional: non-service edit support is incremental.
func DiffProfiles(current, proposed *core.ConnectionProfile) (ProfileDelta, error) {
	var delta ProfileDelta
	if current == nil || proposed == nil {
		return delta, fmt.Errorf("both the current and proposed profiles are required")
	}
	if current.Kind != proposed.Kind {
		return delta, core.ErrValidation(fmt.Sprintf(
			"changing the connection kind from %q to %q is not an edit; clone the connection and delete the original instead",
			current.Kind, proposed.Kind))
	}

	if current.Name != proposed.Name {
		delta.Changes = append(delta.Changes, "name")
	}

	if current.Driver.ProviderID != proposed.Driver.ProviderID {
		delta.Changes = append(delta.Changes, "provider")
		delta.RestartConnector = true
		delta.invalidate(core.ResourceTunnel, core.ResourceDNSRecord, core.ResourceAccessApp, core.ResourceAccessPolicy)
	}
	if current.Driver.AccountID != proposed.Driver.AccountID {
		delta.Changes = append(delta.Changes, "account")
		delta.RestartConnector = true
		delta.invalidate(core.ResourceTunnel, core.ResourceDNSRecord, core.ResourceAccessApp, core.ResourceAccessPolicy)
	}

	switch current.Kind {
	case core.ConnectionServiceExposure:
		cur, prop := current.Spec.ServiceExposure, proposed.Spec.ServiceExposure
		if cur != nil && prop != nil {
			if !sameSource(cur.Source, prop.Source) {
				delta.Changes = append(delta.Changes, "source")
				delta.RestartConnector = true
			}
			if cur.Exposure.Mode != prop.Exposure.Mode {
				delta.Changes = append(delta.Changes, "exposure mode")
				delta.RestartConnector = true
				delta.invalidate(core.ResourceTunnel, core.ResourceDNSRecord, core.ResourceAccessApp, core.ResourceAccessPolicy)
			} else if cur.Exposure.RequestedAddress != prop.Exposure.RequestedAddress {
				delta.Changes = append(delta.Changes, "hostname")
				delta.RestartConnector = true
				delta.invalidate(core.ResourceDNSRecord, core.ResourceAccessApp, core.ResourceAccessPolicy)
			}
			if cur.Exposure.Protocol != prop.Exposure.Protocol {
				delta.Changes = append(delta.Changes, "protocol")
				delta.RestartConnector = true
			}
			if !sameProtection(cur.Protection, prop.Protection) {
				delta.Changes = append(delta.Changes, "protection")
				delta.RestartConnector = true
				delta.invalidate(core.ResourceAccessApp, core.ResourceAccessPolicy)
			}
		}
	case core.ConnectionPortForward:
		cur, prop := current.Spec.PortForward, proposed.Spec.PortForward
		if cur != nil && prop != nil {
			if cur.LocalPort != prop.LocalPort {
				delta.Changes = append(delta.Changes, "local port")
				delta.RestartConnector = true
			}
			if cur.RemoteHost != prop.RemoteHost || cur.RemotePort != prop.RemotePort {
				delta.Changes = append(delta.Changes, "remote target")
				delta.RestartConnector = true
			}
			if cur.Protocol != prop.Protocol {
				delta.Changes = append(delta.Changes, "protocol")
				delta.RestartConnector = true
			}
			if cur.Direction != prop.Direction {
				delta.Changes = append(delta.Changes, "direction")
				delta.RestartConnector = true
			}
		}
	case core.ConnectionPrivateNetwork:
		cur, prop := current.Spec.PrivateNetwork, proposed.Spec.PrivateNetwork
		if cur != nil && prop != nil {
			if cur.NetworkID != prop.NetworkID {
				delta.Changes = append(delta.Changes, "network")
				delta.RestartConnector = true
			}
			if cur.Mode != prop.Mode {
				delta.Changes = append(delta.Changes, "mode")
				delta.RestartConnector = true
			}
		}
	case core.ConnectionClientTunnel:
		cur, prop := current.Spec.ClientTunnel, proposed.Spec.ClientTunnel
		if cur != nil && prop != nil {
			if cur.Client != prop.Client {
				delta.Changes = append(delta.Changes, "client")
				delta.RestartConnector = true
			}
			if cur.TunnelID != prop.TunnelID {
				delta.Changes = append(delta.Changes, "tunnel ID")
				delta.RestartConnector = true
			}
		}
	}

	if current.Lifecycle != proposed.Lifecycle {
		delta.Changes = append(delta.Changes, "lifecycle")
	}
	return delta, nil
}

func sameSource(a, b core.SourceSpec) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch {
	case a.Existing != nil && b.Existing != nil:
		return *a.Existing == *b.Existing
	case a.Directory != nil && b.Directory != nil:
		return *a.Directory == *b.Directory
	case a.Command != nil && b.Command != nil:
		return sameCommand(a.Command, b.Command)
	case a.MCP != nil && b.MCP != nil:
		if a.MCP.Transport != b.MCP.Transport || a.MCP.Endpoint != b.MCP.Endpoint {
			return false
		}
		return sameCommand(a.MCP.Command, b.MCP.Command)
	}
	return a.Existing == nil && b.Existing == nil &&
		a.Directory == nil && b.Directory == nil &&
		a.Command == nil && b.Command == nil &&
		a.MCP == nil && b.MCP == nil
}

func sameCommand(a, b *core.CommandSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Executable != b.Executable || a.WorkingDir != b.WorkingDir ||
		a.Port != b.Port || a.Protocol != b.Protocol || a.UseShell != b.UseShell {
		return false
	}
	if len(a.Args) != len(b.Args) {
		return false
	}
	for i := range a.Args {
		if a.Args[i] != b.Args[i] {
			return false
		}
	}
	if len(a.Env) != len(b.Env) {
		return false
	}
	for k, v := range a.Env {
		if b.Env[k] != v {
			return false
		}
	}
	return true
}

func sameProtection(a, b core.ProtectionSpec) bool {
	if a.Kind != b.Kind || a.SessionTTL != b.SessionTTL {
		return false
	}
	return sameStrings(a.AllowedEmails, b.AllowedEmails) && sameStrings(a.AllowedDomains, b.AllowedDomains)
}

func sameStrings(a, b []string) bool {
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

// PlanEdit produces the change plan for editing a connection.
//
// The plan pauses the connection if it is open, removes the provider resources
// the proposed profile no longer describes, commits the new profile, and
// reopens if it was open. The previous profile stays committed until the apply
// step runs, so a failure before that point leaves the connection exactly as it
// was.
func (c *Controller) PlanEdit(ctx context.Context, connID core.ConnectionID, proposed *core.ConnectionProfile) (*core.OperationPlan, ProfileDelta, error) {
	c.mu.RLock()
	current, ok := c.profiles[connID]
	runtime := c.runtimes[connID]
	c.mu.RUnlock()

	var delta ProfileDelta
	if !ok {
		return nil, delta, core.ErrProfileNotFound(connID)
	}
	if proposed == nil {
		return nil, delta, core.ErrValidation("a proposed profile is required")
	}

	// The proposed profile keeps the connection's identity; an edit cannot
	// reassign it.
	candidate := proposed.DeepCopy()
	candidate.ID = current.ID
	candidate.Revision = current.Revision
	candidate.Desired = current.Desired
	candidate.CreatedAt = current.CreatedAt

	if err := candidate.Validate(); err != nil {
		return nil, delta, core.ErrValidation(fmt.Sprintf("the proposed connection is not valid: %v", err))
	}

	delta, err := DiffProfiles(current, candidate)
	if err != nil {
		return nil, delta, err
	}

	// Validate provider compatibility for the proposed profile.
	if err := c.validateProviderForProfile(ctx, candidate); err != nil {
		return nil, delta, err
	}

	plan := &core.OperationPlan{
		ID:              core.NewPlanID(),
		ConnectionID:    connID,
		ProfileRevision: current.Revision,
		Provider:        candidate.Driver.ProviderID,
		Intent:          core.IntentEdit,
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}

	// Reject provider/account changes as edits. The current plan model cannot
	// safely represent mixed-provider execution. Direct users to clone+delete.
	if current.Driver.ProviderID != candidate.Driver.ProviderID {
		return nil, delta, core.ErrValidation(fmt.Sprintf(
			"changing the provider from %q to %q is not an edit; clone the connection and delete the original instead",
			current.Driver.ProviderID, candidate.Driver.ProviderID))
	}
	if current.Driver.AccountID != candidate.Driver.AccountID {
		return nil, delta, core.ErrValidation(fmt.Sprintf(
			"changing the account from %q to %q is not an edit; clone the connection and delete the original instead",
			current.Driver.AccountID, candidate.Driver.AccountID))
	}

	// A no-op edit must not close and reopen a working connection.
	if delta.Empty() {
		if err := plan.ComputeFingerprint(); err != nil {
			return nil, delta, err
		}
		return plan, delta, nil
	}

	wasOpen := current.Desired == core.DesiredOpen &&
		runtime != nil && runtime.State != core.RuntimeClosed

	if wasOpen && delta.RestartConnector {
		plan.Steps = append(plan.Steps, core.PlanStep{
			ID: "edit-stop-connector", Kind: core.StepStopConnector,
			Summary:   "Pause the connection",
			Technical: core.TechnicalOperation{Provider: current.Driver.ProviderID, Type: "stop_connector"},
		})
	}

	// Delete only resources Portico owns. An adopted or external resource is
	// left alone: Portico did not create it and must not remove it.
	for _, resource := range c.resourcesForEdit(connID, runtime) {
		if !delta.InvalidatedResources[resource.Type] {
			continue
		}
		if resource.Ownership != core.OwnershipManaged {
			plan.Warnings = append(plan.Warnings, core.PlanWarning{
				Code: "PTO-EDIT-ADOPTED",
				Message: fmt.Sprintf(
					"%s %s is no longer described by this connection but was not created by Portico, so it is left in place",
					resource.Type, resource.ExternalID),
			})
			continue
		}
		step, ok := deleteStepForResource(current.Driver.ProviderID, resource)
		if !ok {
			continue
		}
		plan.Steps = append(plan.Steps, step)
	}

	// The commit boundary. Everything before this leaves the old profile in
	// place; everything after it operates on the new one.
	plan.Steps = append(plan.Steps, core.PlanStep{
		ID: "edit-apply-profile", Kind: core.StepApplyProfile,
		Summary:   "Apply the edited connection",
		Technical: core.TechnicalOperation{Type: "apply_profile"},
	})

	// Expected state after the operation: preserve current runtime state
	// for metadata-only edits, restore to open if we stopped and reopen.
	if wasOpen {
		plan.Expected.State = core.RuntimeOpen
	} else {
		plan.Expected.State = core.RuntimeClosed
	}

	if wasOpen && delta.RestartConnector {
		openPlan, err := c.planOpenSteps(ctx, candidate)
		if err != nil {
			return nil, delta, fmt.Errorf("plan reopen after edit: %w", err)
		}
		plan.Steps = append(plan.Steps, openPlan...)
	}

	if err := plan.ComputeFingerprint(); err != nil {
		return nil, delta, err
	}
	// Embed the proposed profile in the plan as a durable, fingerprint-bound
	// payload. This eliminates pendingEdits and makes edit plans survive
	// supervisor restart.
	editPayload, err := core.NewEditPayload(candidate)
	if err != nil {
		return nil, delta, fmt.Errorf("create edit payload: %w", err)
	}
	plan.EditPayload = editPayload
	// Recompute fingerprint to include the edit payload hash.
	if err := plan.ComputeFingerprint(); err != nil {
		return nil, delta, err
	}
	return plan, delta, nil
}

// RebindPendingEdit is retained for backward compatibility as a no-op.
func (c *Controller) RebindPendingEdit(from, to core.PlanID) {
}

// resourcesForEdit returns the tracked provider resources for a connection.
func (c *Controller) resourcesForEdit(connID core.ConnectionID, runtime *core.ConnectionRuntime) []core.ProviderResource {
	if runtime == nil {
		return nil
	}
	var live []core.ProviderResource
	for _, r := range runtime.Provider.Resources {
		switch r.Lifecycle {
		case core.LifecycleRemoved, core.LifecycleExternallyRemoved:
			continue
		}
		live = append(live, r)
	}
	return live
}

// deleteStepForResource builds the deletion step for a resource type.
func deleteStepForResource(providerID core.ProviderID, resource core.ProviderResource) (core.PlanStep, bool) {
	var kind core.StepKind
	var operation string
	switch resource.Type {
	case core.ResourceTunnel:
		kind, operation = core.StepDeleteTunnel, "delete_tunnel"
	case core.ResourceDNSRecord:
		kind, operation = core.StepDeleteDNSRecord, "delete_dns"
	case core.ResourceAccessApp:
		kind, operation = core.StepDeleteAccessApp, "delete_access"
	case core.ResourceAccessPolicy:
		kind, operation = core.StepDeleteAccessPolicy, "delete_access_policy"
	default:
		return core.PlanStep{}, false
	}
	return core.PlanStep{
		ID:      fmt.Sprintf("edit-delete-%s-%s", resource.Type, safeResourceID(resource.ExternalID)),
		Kind:    kind,
		Summary: fmt.Sprintf("Remove the %s this edit replaces", resource.Type),
		Technical: core.TechnicalOperation{
			Provider:   providerID,
			Type:       operation,
			ResourceID: resource.ExternalID,
		},
		Destructive: true,
		Ownership:   core.OwnershipManaged,
		// Compensation: recreate the resource on the old profile if a later
		// step fails. This makes the edit all-or-nothing: either the new
		// profile is fully applied, or the connection is left exactly as it
		// was. The compensation step uses the old provider binding because
		// the resource was created under the old profile.
		Compensation: &core.CompensationStep{
			ID:   fmt.Sprintf("edit-recreate-%s-%s", resource.Type, safeResourceID(resource.ExternalID)),
			Kind: kind,
			Technical: core.TechnicalOperation{
				Provider:   providerID,
				Type:       operation, // The provider's delete is reversible only if it tracks external IDs; otherwise this is a no-op that records the obligation.
				ResourceID: resource.ExternalID,
			},
		},
	}, true
}

func safeResourceID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// planOpenSteps produces the steps that would open a connection under the
// given profile, without touching the stored profile.
//
// It exists so an edit can append the reopen to its own plan. The profile is
// passed explicitly rather than read from the controller, because during an
// edit the stored profile is still the previous one.
func (c *Controller) planOpenSteps(ctx context.Context, profile *core.ConnectionProfile) ([]core.PlanStep, error) {
	prov, err := c.providerForProfile(profile)
	if err != nil {
		return nil, err
	}

	openProfile := profile.DeepCopy()
	openProfile.Desired = core.DesiredOpen

	// Only service-exposure connections have a local origin model. Port forwards,
	// client tunnels, and private networks have no service origin — pass nil.
	var resolvedOrigin *core.ResolvedOrigin
	if openProfile.Kind == core.ConnectionServiceExposure {
		sourceSpec := openProfile.GetSource()
		var err error
		resolvedOrigin, err = c.prepareOriginForConnection(ctx, profile.ID, sourceSpec)
		if err != nil {
			return nil, fmt.Errorf("origin preparation: %w", err)
		}
	}

	plan, err := prov.Plan(ctx, core.DesiredConnection{
		Profile: openProfile,
		Origin:  resolvedOrigin,
	})
	if err != nil {
		return nil, err
	}
	if resolvedOrigin != nil && resolvedOrigin.Owned {
		insertStartOriginStep(plan, resolvedOrigin.URL)
	}
	return plan.Steps, nil
}
