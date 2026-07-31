package supervisor

import (
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

// describeSpec projects a connection profile's spec union onto the wire.
//
// It populates the one arm the profile actually has. The previous builder read
// GetSource/GetExposure/GetProtection unconditionally; those accessors return
// zero values for every kind except service exposure, so a port forward was
// described as a service exposure whose source was blank, whose exposure mode
// was blank and whose protection was "" — a shape no reader could distinguish
// from a misconfigured public tunnel.
func describeSpec(p *core.ConnectionProfile) ipc.ConnectionSpecDTO {
	if p == nil {
		return ipc.ConnectionSpecDTO{}
	}

	kind := p.EffectiveKind()
	spec := ipc.ConnectionSpecDTO{Kind: string(kind)}

	// Dispatch on the same authority computeRouteSegments uses. Switching on
	// which arm is populated instead would let a profile be routed as one kind
	// and described as another whenever the two disagree.
	switch kind {
	case core.ConnectionPortForward:
		pf := p.Spec.PortForward
		if pf == nil {
			return spec
		}
		spec.PortForward = &ipc.PortForwardDTO{
			LocalPort:  pf.LocalPort,
			RemoteHost: pf.RemoteHost,
			RemotePort: pf.RemotePort,
			Protocol:   string(pf.Protocol),
			Direction:  string(pf.Direction),
		}

	case core.ConnectionPrivateNetwork:
		pn := p.Spec.PrivateNetwork
		if pn == nil {
			return spec
		}
		spec.PrivateNetwork = &ipc.PrivateNetworkSpecDTO{
			NetworkID:   pn.NetworkID,
			Mode:        string(pn.Mode),
			ExposeLocal: pn.ExposeLocal,
		}

	case core.ConnectionClientTunnel:
		ct := p.Spec.ClientTunnel
		if ct == nil {
			return spec
		}
		spec.ClientTunnel = &ipc.ClientTunnelSpecDTO{
			Client:   string(ct.Client),
			TunnelID: ct.TunnelID,
			Profile:  ct.Profile,
			MCP:      describeMCP(&ct.MCP),
		}

	default:
		se := p.Spec.ServiceExposure
		if se == nil {
			return spec
		}
		spec.ServiceExposure = &ipc.ServiceExposureSpecDTO{
			Source: describeSource(se.Source),
			Exposure: ipc.ExposureDTO{
				Mode:             string(se.Exposure.Mode),
				Protocol:         string(se.Exposure.Protocol),
				RequestedAddress: se.Exposure.RequestedAddress,
			},
			Protection: ipc.ProtectionDTO{
				Kind:           string(se.Protection.Kind),
				AllowedEmails:  se.Protection.AllowedEmails,
				AllowedDomains: se.Protection.AllowedDomains,
			},
		}
	}

	return spec
}

// describeSource projects the source union of a service exposure.
func describeSource(src core.SourceSpec) ipc.SourceDTO {
	dto := ipc.SourceDTO{Kind: string(src.Kind)}
	if src.Existing != nil {
		dto.Existing = &ipc.ExistingSourceDTO{Address: src.Existing.Address}
	}
	if src.Directory != nil {
		dto.Directory = &ipc.DirectorySourceDTO{
			Path:        src.Directory.Path,
			Mode:        string(src.Directory.Mode),
			SPAFallback: src.Directory.SPAFallback,
			AllowUpload: src.Directory.AllowUpload,
			AllowDelete: src.Directory.AllowDelete,
		}
	}
	if src.Command != nil {
		dto.Command = describeCommand(src.Command)
	}
	if src.MCP != nil {
		mcp := describeMCP(src.MCP)
		dto.MCP = &mcp
	}
	return dto
}

// describeMCP projects an MCP service spec.
func describeMCP(mcp *core.MCPServiceSpec) ipc.MCPSourceDTO {
	if mcp == nil {
		return ipc.MCPSourceDTO{}
	}
	return ipc.MCPSourceDTO{
		Transport: string(mcp.Transport),
		Endpoint:  mcp.Endpoint,
		Command:   describeCommand(mcp.Command),
	}
}

// describeCommand projects a command spec.
//
// Env is deliberately omitted: it is operator-supplied and routinely holds
// tokens, and a detail view is rendered, logged and included in support
// exports. The command line is reported; its environment is not.
func describeCommand(cmd *core.CommandSpec) *ipc.CommandSourceDTO {
	if cmd == nil {
		return nil
	}
	return &ipc.CommandSourceDTO{
		Executable: cmd.Executable,
		Args:       cmd.Args,
		WorkingDir: cmd.WorkingDir,
		Port:       cmd.Port,
		Protocol:   string(cmd.Protocol),
		UseShell:   cmd.UseShell,
	}
}

// connectionSummaryDTO projects a profile and its runtime onto the summary DTO.
//
// This existed twice — once for the snapshot list and once for the detail
// view — and the two copies had to be kept in agreement by hand. They did not
// stay in agreement: adding the connection kind to one left the other reporting
// every connection as kindless, which readers resolve to "published service".
func connectionSummaryDTO(p *core.ConnectionProfile, rt *core.ConnectionRuntime) ipc.ConnectionDTO {
	if p == nil {
		return ipc.ConnectionDTO{}
	}
	dto := ipc.ConnectionDTO{
		ID:                string(p.ID),
		Name:              p.Name,
		Kind:              string(p.EffectiveKind()),
		DesiredState:      string(p.Desired),
		ProviderID:        string(p.GetProvider().ProviderID),
		ProviderAccountID: string(p.GetProvider().AccountID),
	}
	if rt != nil {
		dto.RuntimeState = string(rt.State)
		dto.UserState = rt.State.UserFacingState()
		dto.PublicAddress = rt.Endpoint.PublicAddress
		dto.PrivateAddress = rt.Endpoint.PrivateAddress
		dto.ConnectorPID = rt.Connector.PID
		dto.ConnectorState = string(rt.Connector.Status)
		if rt.Error != nil {
			dto.Error = rt.Error.Message
		}
	}
	return dto
}
