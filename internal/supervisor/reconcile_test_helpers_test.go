package supervisor

import "github.com/B-A-M-N/portico/internal/core"

// networkProfile is a private-network connection in the given mode.
func networkProfile(id core.ConnectionID, mode core.PrivateNetworkMode, address string) *core.ConnectionProfile {
	spec := &core.PrivateNetworkSpec{
		NetworkID: "example.com", Mode: mode,
		ExposeLocal:  mode == core.PrivateNetworkExpose,
		LocalAddress: address,
	}
	if mode == core.PrivateNetworkExpose {
		spec.LocalProtocol = core.ProtocolHTTP
	}
	return &core.ConnectionProfile{
		ID: id, Name: "network", Kind: core.ConnectionPrivateNetwork,
		Desired: core.DesiredOpen, Revision: 1,
		Driver: core.DriverSelection{ProviderID: "tailscale"},
		Spec:   core.ConnectionSpec{PrivateNetwork: spec},
	}
}
