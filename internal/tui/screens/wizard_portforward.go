package screens

// Port-forward protocol.
//
// The protocol menu offered "TCP" and "UDP (not implemented)" as two apparently
// equal items, and refused UDP with an error only after the user chose it. UDP
// is not a preference Portico is withholding: no origin backend, no relay and no
// provider path carries it, so it cannot be delivered at all.
//
// Rendering it through the same wizardChoice type every other menu uses means it
// is listed, visibly unavailable, and carries its reason before the user spends a
// keystroke on it — the same treatment an exposure mode or a protection policy
// gets.

// portForwardProtocolChoices are the transport protocols a forward can use.
func portForwardProtocolChoices() []wizardChoice {
	return []wizardChoice{
		{
			Value:     "tcp",
			Label:     "TCP",
			Available: true,
			Detail: []string{
				"Portico listens on the local port and relays each connection to the remote host.",
			},
		},
		{
			Value:     "udp",
			Label:     "UDP",
			Available: false,
			Reason:    "Portico cannot forward UDP yet",
			Detail: []string{
				"UDP forwarding is not implemented: the relay is connection-oriented and there is",
				"no datagram path through it. Nothing about your configuration is preventing this.",
			},
		},
	}
}
