package supervisor

import (
	"fmt"
	"strings"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

// planToDTO converts an operation plan into its preview form.
//
// A preview that lists only step summaries makes the user reconstruct the
// consequences themselves. This describes the outcome, who will have access,
// what changes locally, what is created at the provider, and what is reversible
// — with the exact step list retained as the technical plan.
//
// The profile may be nil, in which case only the plan-derived sections are
// produced; nothing is invented from an absent profile.
func planToDTO(plan *core.OperationPlan, profile *core.ConnectionProfile) *ipc.PlanDTO {
	if plan == nil {
		return nil
	}

	dto := &ipc.PlanDTO{
		ID:           string(plan.ID),
		ConnectionID: string(plan.ConnectionID),
		Intent:       string(plan.Intent),
		Provider:     string(plan.Provider),
		Fingerprint:  plan.Fingerprint,
	}
	for _, step := range plan.Steps {
		dto.Steps = append(dto.Steps, ipc.StepDTO{
			ID:           step.ID,
			Kind:         string(step.Kind),
			Summary:      step.Summary,
			Destructive:  step.Destructive,
			Irreversible: step.Irreversible,
		})
	}
	for _, warning := range plan.Warnings {
		dto.Warnings = append(dto.Warnings, warning.Message)
	}

	dto.Outcome = describeOutcome(plan, profile)
	dto.Access = describeAccess(plan, profile)
	dto.LocalChanges = describeLocalChanges(plan)
	dto.ProviderChanges = describeProviderChanges(plan)
	dto.Reversibility = describeReversibility(plan)
	return dto
}

func describeOutcome(plan *core.OperationPlan, profile *core.ConnectionProfile) string {
	switch plan.Intent {
	case core.IntentOpen:
		address := plan.Expected.PublicAddress
		if address == "" {
			address = plan.Expected.PrivateAddress
		}
		origin := originDescription(profile)
		switch {
		case address != "" && origin != "":
			return fmt.Sprintf("Your local service at %s will be available at %s.", origin, address)
		case address != "":
			return fmt.Sprintf("The connection will be available at %s.", address)
		case origin != "":
			return fmt.Sprintf("Your local service at %s will be published.", origin)
		}
		return "The connection will be opened."
	case core.IntentClose:
		return "The connection will stop serving traffic."
	case core.IntentRepair:
		return "Portico will restore the connection to its intended state."
	case core.IntentDelete:
		return "The connection and the provider resources Portico created for it will be removed."
	}
	return ""
}

func originDescription(profile *core.ConnectionProfile) string {
	if profile == nil || profile.Spec.ServiceExposure == nil {
		return ""
	}
	source := profile.Spec.ServiceExposure.Source
	switch {
	case source.Existing != nil && source.Existing.Address != "":
		return source.Existing.Address
	case source.Directory != nil && source.Directory.Path != "":
		return source.Directory.Path
	case source.Command != nil && source.Command.Executable != "":
		return fmt.Sprintf("the %s process", source.Command.Executable)
	case source.MCP != nil:
		return "the MCP server"
	}
	return ""
}

// describeAccess states who will be able to reach the service, in the terms the
// audit asks for: a temporary link is reachable by anyone who has it, and an
// Access policy is reachable only by the identities it names.
func describeAccess(plan *core.OperationPlan, profile *core.ConnectionProfile) string {
	if plan.Intent != core.IntentOpen {
		return ""
	}
	if profile == nil {
		return ""
	}

	// Each kind is reachable by a different set of people, and the preview is
	// where the user decides whether that is what they want. Returning nothing
	// for the three non-exposed kinds left the one question the preview exists
	// to answer unanswered.
	switch profile.EffectiveKind() {
	case core.ConnectionPortForward:
		return describePortForwardAccess(profile.Spec.PortForward)
	case core.ConnectionClientTunnel:
		return describeClientTunnelAccess(profile.Spec.ClientTunnel)
	case core.ConnectionPrivateNetwork:
		return describePrivateNetworkAccess(profile.Spec.PrivateNetwork)
	}

	spec := profile.Spec.ServiceExposure
	if spec == nil {
		return ""
	}

	if spec.Protection.Kind == "" || spec.Protection.Kind == core.ProtectionNone {
		if spec.Exposure.Mode == core.ExposureTemporary {
			return "Anyone with this temporary link can reach the service while the connection is open."
		}
		return "Anyone with this address can reach the service."
	}

	var allowed []string
	allowed = append(allowed, spec.Protection.AllowedEmails...)
	for _, domain := range spec.Protection.AllowedDomains {
		allowed = append(allowed, "anyone at "+domain)
	}
	if len(allowed) == 0 {
		return "Access is restricted, but no identities are listed yet."
	}
	return fmt.Sprintf("Only %s can sign in.", joinWithAnd(allowed))
}

// describePortForwardAccess states who can reach a forward, derived from the
// bind address rather than assumed in either direction.
func describePortForwardAccess(spec *core.PortForwardSpec) string {
	if spec == nil {
		return ""
	}
	if spec.Direction == core.PortForwardRemote {
		return fmt.Sprintf(
			"Traffic arriving at %s on the remote side will be delivered to port %d on this machine.",
			joinHostPort(spec.RemoteHost, spec.RemotePort), spec.LocalPort)
	}
	return fmt.Sprintf(
		"Only this machine can use 127.0.0.1:%d; traffic sent there reaches %s. Nothing is published.",
		spec.LocalPort, joinHostPort(spec.RemoteHost, spec.RemotePort))
}

// describeClientTunnelAccess states the defining property of the kind: nothing
// is published and no address is created.
func describeClientTunnelAccess(spec *core.ClientTunnelSpec) string {
	if spec == nil {
		return ""
	}
	return fmt.Sprintf(
		"Only %s can reach this server, through the tunnel client running on this machine. "+
			"No public address is created and no inbound port is opened.",
		clientPlatform(spec.Client))
}

// describePrivateNetworkAccess states that reachability is membership.
func describePrivateNetworkAccess(spec *core.PrivateNetworkSpec) string {
	if spec == nil {
		return ""
	}
	network := spec.NetworkID
	if network == "" {
		network = "the private network"
	}
	if spec.Mode == core.PrivateNetworkExpose || spec.ExposeLocal {
		return fmt.Sprintf("Only members of %s can reach this service. It is not published to the internet.", network)
	}
	return fmt.Sprintf("This machine joins %s. Nothing local is published.", network)
}

func describeLocalChanges(plan *core.OperationPlan) []string {
	var changes []string
	for _, step := range plan.Steps {
		switch step.Kind {
		case core.StepStartOrigin:
			changes = append(changes, "Portico will start and supervise the local service.")
		case core.StepStopOrigin:
			changes = append(changes, "Portico will stop the local service it started.")
		case core.StepStartConnector, core.StepRestartConnector:
			changes = append(changes, "Portico will start and supervise one connector process.")
		case core.StepStopConnector:
			changes = append(changes, "Portico will stop the connector process.")
		}
	}
	return dedupe(changes)
}

func describeProviderChanges(plan *core.OperationPlan) []string {
	var changes []string
	for _, step := range plan.Steps {
		switch step.Kind {
		case core.StepCreateTunnel, core.StepRecreateTunnel:
			changes = append(changes, "Create one managed tunnel.")
		case core.StepCreateDNSRecord:
			changes = append(changes, "Create one DNS record.")
		case core.StepUpdateDNSRecord:
			changes = append(changes, "Update one DNS record.")
		case core.StepCreateAccessApp, core.StepCreateAccessPolicy:
			changes = append(changes, "Create one access policy.")
		case core.StepUpdateAccessApp, core.StepUpdateAccessPolicy:
			changes = append(changes, "Update one access policy.")
		case core.StepDeleteTunnel:
			changes = append(changes, "Delete the managed tunnel.")
		case core.StepDeleteDNSRecord:
			changes = append(changes, "Delete the DNS record.")
		case core.StepDeleteAccessApp, core.StepDeleteAccessPolicy:
			changes = append(changes, "Delete the access policy.")
		}
	}
	if len(changes) == 0 {
		return []string{"No provider resources will be created or removed."}
	}
	return dedupe(changes)
}

func describeReversibility(plan *core.OperationPlan) []string {
	var notes []string
	switch plan.Intent {
	case core.IntentOpen:
		notes = append(notes,
			"Closing stops the connector but keeps the provider resources Portico created.",
			"Deleting removes the provider resources Portico created.")
	case core.IntentClose:
		notes = append(notes, "Reopening this connection does not need to recreate provider resources.")
	case core.IntentDelete:
		notes = append(notes, "This cannot be undone. Provider resources Portico created will be removed.")
	}
	for _, step := range plan.Steps {
		if step.Irreversible {
			notes = append(notes, fmt.Sprintf("Step %q cannot be undone once it has run.", step.Summary))
		}
	}
	return dedupe(notes)
}

func joinWithAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

func dedupe(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}
