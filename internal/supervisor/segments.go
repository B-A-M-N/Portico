package supervisor

import (
	"fmt"
	"net"
	"strings"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

// Segment status values. A segment reports what the available evidence
// supports and nothing more: "unknown" is a legitimate answer and is never
// upgraded to "ok" merely because no failure was recorded.
const (
	segmentOK        = "ok"
	segmentDegraded  = "degraded"
	segmentFailed    = "failed"
	segmentUnknown   = "unknown"
	segmentNotInUse  = "not_applicable"
	segmentPending   = "pending"
	segmentNotOpened = "not_opened"
)

// computeRouteSegments describes the connection as a chain of segments rather
// than a single open/failed verdict.
//
// A connection that is "failed" tells the user nothing about where. Reporting
// per-segment state lets Portico say which hop is broken — the local service,
// the connector, the provider resource, DNS, protection, or the endpoint — and
// which hops are known good.
//
// The chain is chosen by connection kind. It used to be written for service
// exposure alone: it read Spec.ServiceExposure and, finding nil for the other
// three kinds, took the else branch of every decision. A port forward bound to
// loopback was therefore reported as sitting on a temporary address, behind a
// provider tunnel, with a public endpoint, and — worst — as "not protected;
// anyone with the address can reach it". A connection that nothing outside the
// machine can reach was described as the most exposed thing Portico can build.
func computeRouteSegments(
	profile *core.ConnectionProfile,
	rt *core.ConnectionRuntime,
	resources []core.ProviderResource,
) []ipc.RouteSegmentDTO {
	if profile == nil {
		return nil
	}

	b := &routeBuilder{
		profile:   profile,
		rt:        rt,
		resources: resources,
		closed:    profile.Desired == core.DesiredClosed,
		findings:  map[core.RouteSegmentID]core.DiagnosticFinding{},
	}
	// Findings are the authoritative record of a diagnosed fault, so a segment
	// with an open finding is reported from that rather than inferred.
	if rt != nil {
		for _, f := range rt.Diagnostics {
			if f.ResolvedAt == nil {
				b.findings[f.Segment] = f
			}
		}
	}

	switch profile.EffectiveKind() {
	case core.ConnectionPortForward:
		return b.portForwardRoute()
	case core.ConnectionClientTunnel:
		return b.clientTunnelRoute()
	case core.ConnectionPrivateNetwork:
		return b.privateNetworkRoute()
	default:
		return b.serviceExposureRoute()
	}
}

type routeBuilder struct {
	profile   *core.ConnectionProfile
	rt        *core.ConnectionRuntime
	resources []core.ProviderResource
	closed    bool
	findings  map[core.RouteSegmentID]core.DiagnosticFinding
}

// segment builds one hop, letting a diagnosed fault override what was inferred.
func (b *routeBuilder) segment(id core.RouteSegmentID, label, status, detail string) ipc.RouteSegmentDTO {
	seg := ipc.RouteSegmentDTO{ID: string(id), Label: label, Status: status, Error: detail}
	if f, ok := b.findings[id]; ok {
		seg.Status = segmentFailed
		if f.Severity == core.SeverityWarning {
			seg.Status = segmentDegraded
		}
		seg.Error = f.Summary
	}
	return seg
}

// connectorSegment is the one hop every kind has: the process Portico runs.
func (b *routeBuilder) connectorSegment(label string) ipc.RouteSegmentDTO {
	status, detail := segmentUnknown, ""
	if b.rt != nil {
		switch b.rt.Connector.Status {
		case core.ConnectorStatusRunning:
			status = segmentOK
			detail = fmt.Sprintf("PID %d", b.rt.Connector.PID)
		case core.ConnectorStatusStarting:
			status = segmentPending
		case core.ConnectorStatusStopped:
			if b.closed {
				status = segmentNotOpened
			} else {
				status = segmentFailed
				detail = "the connector is not running"
			}
		case core.ConnectorStatusUnstable:
			status = segmentDegraded
			detail = fmt.Sprintf("restarted %d times", b.rt.Connector.Restarts)
		case core.ConnectorStatusCrashed:
			status = segmentFailed
			detail = b.rt.Connector.LastError
		}
	}
	return b.segment(core.SegmentConnector, label, status, detail)
}

// connectorBackedStatus reports a hop whose only evidence is that the process
// carrying it is running. It is used for hops Portico does not probe
// independently, and reports unknown rather than ok when it has no evidence.
func (b *routeBuilder) connectorBackedStatus() string {
	switch {
	case b.closed:
		return segmentNotOpened
	case b.rt != nil && b.rt.Connector.Status == core.ConnectorStatusRunning:
		return segmentOK
	default:
		return segmentUnknown
	}
}

// --------------- service exposure ---------------

func (b *routeBuilder) serviceExposureRoute() []ipc.RouteSegmentDTO {
	spec := b.profile.Spec.ServiceExposure
	var segments []ipc.RouteSegmentDTO

	originLabel := "Local service"
	if origin := originDescription(b.profile); origin != "" {
		originLabel = "Local service at " + origin
	}
	segments = append(segments, b.segment(core.SegmentLocalService, originLabel, segmentUnknown,
		"Portico has not probed the local service since the last change."))

	segments = append(segments, b.connectorSegment("Connector process"))

	// Provider edge: the managed tunnel.
	segments = append(segments, b.segment(core.SegmentProviderEdge, "Provider tunnel",
		resourceStatus(b.resources, core.ResourceTunnel, b.closed),
		resourceDetail(b.resources, core.ResourceTunnel)))

	// Address: a DNS record only exists for a permanent hostname.
	if spec != nil && spec.Exposure.Mode == core.ExposurePermanent {
		segments = append(segments, b.segment(core.SegmentAddress,
			"DNS record for "+spec.Exposure.RequestedAddress,
			resourceStatus(b.resources, core.ResourceDNSRecord, b.closed),
			resourceDetail(b.resources, core.ResourceDNSRecord)))
	} else {
		segments = append(segments, b.segment(core.SegmentAddress, "Address",
			segmentNotInUse, "this connection uses a temporary address"))
	}

	// Protection applies only when the profile asks for it.
	if b.profile.IsProtected() {
		segments = append(segments, b.segment(core.SegmentProtection,
			fmt.Sprintf("Access protection (%s)", spec.Protection.Kind),
			resourceStatus(b.resources, core.ResourceAccessApp, b.closed),
			resourceDetail(b.resources, core.ResourceAccessApp)))
	} else {
		segments = append(segments, b.segment(core.SegmentProtection, "Access protection",
			segmentNotInUse, "this connection is not protected; anyone with the address can reach it"))
	}

	// Public endpoint.
	endpointStatus, endpointDetail := segmentUnknown, ""
	switch {
	case b.closed:
		endpointStatus, endpointDetail = segmentNotOpened, "the connection is closed"
	case b.rt != nil && b.rt.Endpoint.PublicAddress != "":
		endpointStatus = segmentOK
		endpointDetail = b.rt.Endpoint.PublicAddress
	case b.rt != nil && b.rt.State == core.RuntimeError:
		endpointStatus, endpointDetail = segmentFailed, "no public address was established"
	}
	segments = append(segments, b.segment(core.SegmentEndpoint, "Public endpoint", endpointStatus, endpointDetail))

	return segments
}

// --------------- port forward ---------------

// portForwardRoute describes a forward as what it is: a listener, the process
// carrying the traffic, and the endpoint on the other side. There is no
// provider resource, no address to register and no access policy, so those hops
// are absent rather than reported as inapplicable with exposure wording.
func (b *routeBuilder) portForwardRoute() []ipc.RouteSegmentDTO {
	spec := b.profile.Spec.PortForward
	if spec == nil {
		spec = &core.PortForwardSpec{}
	}
	remote := joinHostPort(spec.RemoteHost, spec.RemotePort)
	listener := b.listenAddress(spec)
	status := b.connectorBackedStatus()

	if spec.Direction == core.PortForwardRemote {
		// The listener is on the far side; traffic arrives from there and is
		// delivered to a local port.
		return []ipc.RouteSegmentDTO{
			b.segment(core.SegmentEndpoint, "Remote listener on "+remote, status,
				"opened by the provider on the remote side"),
			b.connectorSegment("Forwarding process"),
			b.segment(core.SegmentLocalService, fmt.Sprintf("Local service on port %d", spec.LocalPort),
				segmentUnknown, "Portico has not probed the local service since the last change."),
		}
	}

	return []ipc.RouteSegmentDTO{
		b.segment(core.SegmentLocalRoute, "Local listener on "+listener, status, b.listenerReach(listener)),
		b.connectorSegment("Forwarding process"),
		b.segment(core.SegmentEndpoint, "Remote endpoint "+remote, status,
			"traffic to the local listener is delivered here"),
	}
}

// listenAddress reports where the forward listens, preferring the address the
// running forward actually reported over the one the spec asked for.
func (b *routeBuilder) listenAddress(spec *core.PortForwardSpec) string {
	if b.rt != nil && b.rt.Endpoint.PrivateAddress != "" {
		return b.rt.Endpoint.PrivateAddress
	}
	return fmt.Sprintf("127.0.0.1:%d", spec.LocalPort)
}

// listenerReach states who can reach the listener, which is the question the
// exposure wording used to answer wrongly. It is derived from the bind address
// rather than assumed: a forward bound to a routable interface is reachable
// from the network, and saying otherwise would be the same defect inverted.
func (b *routeBuilder) listenerReach(listener string) string {
	host, _, err := net.SplitHostPort(listener)
	if err != nil {
		host = listener
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "reachable only from this machine"
	}
	if host == "localhost" {
		return "reachable only from this machine"
	}
	if host == "0.0.0.0" || host == "::" {
		return "bound to every interface: reachable from your network"
	}
	return "reachable from wherever " + host + " is routable"
}

// --------------- client tunnel ---------------

// clientTunnelRoute describes a connection whose defining property is that it
// has no public address: an outbound client carries requests from the platform
// to a local server. Naming a public endpoint here would send the user looking
// for a link that will never exist.
func (b *routeBuilder) clientTunnelRoute() []ipc.RouteSegmentDTO {
	spec := b.profile.Spec.ClientTunnel
	if spec == nil {
		spec = &core.ClientTunnelSpec{}
	}

	localLabel := "Local MCP server"
	if spec.MCP.Endpoint != "" {
		localLabel = "Local MCP server at " + spec.MCP.Endpoint
	} else if spec.MCP.Command != nil && spec.MCP.Command.Executable != "" {
		localLabel = "Local MCP server: the " + spec.MCP.Command.Executable + " process"
	}

	// Portico does not create the platform-side tunnel — it is configured in
	// the platform's own settings — so it reports the identity it was given and
	// does not claim to have verified it.
	edgeStatus, edgeDetail := segmentUnknown, "Portico does not create or verify this tunnel"
	switch {
	case b.closed:
		edgeStatus, edgeDetail = segmentNotOpened, "the connection is closed"
	case spec.TunnelID != "":
		edgeDetail = "tunnel " + spec.TunnelID + " is configured in the platform, which Portico cannot verify"
	}

	return []ipc.RouteSegmentDTO{
		b.segment(core.SegmentLocalService, localLabel, segmentUnknown,
			"Portico has not probed the local service since the last change."),
		b.connectorSegment("Tunnel client process"),
		b.segment(core.SegmentProviderEdge, "Platform tunnel", edgeStatus, edgeDetail),
		b.segment(core.SegmentEndpoint, "Reachable from "+clientPlatform(spec.Client),
			b.connectorBackedStatus(),
			"no address is created and no inbound port is opened"),
	}
}

// clientPlatform names the platform whose client mediates the tunnel.
func clientPlatform(kind core.ClientKind) string {
	switch kind {
	case core.ClientOpenAISecureMCPTunnel:
		return "your OpenAI account"
	default:
		return "the connected platform"
	}
}

// --------------- private network ---------------

// privateNetworkRoute describes a connection reachable by network members. Its
// address is private, and its protection is membership rather than an access
// policy, so it is reported as such rather than as an unprotected public route.
func (b *routeBuilder) privateNetworkRoute() []ipc.RouteSegmentDTO {
	spec := b.profile.Spec.PrivateNetwork
	if spec == nil {
		spec = &core.PrivateNetworkSpec{}
	}

	network := spec.NetworkID
	if network == "" {
		network = "the private network"
	}

	var segments []ipc.RouteSegmentDTO

	// A join-only connection carries no local service: the machine becomes a
	// member, and nothing local is published.
	if spec.Mode == core.PrivateNetworkExpose || spec.ExposeLocal {
		originLabel := "Local service"
		if origin := originDescription(b.profile); origin != "" {
			originLabel = "Local service at " + origin
		}
		segments = append(segments, b.segment(core.SegmentLocalService, originLabel, segmentUnknown,
			"Portico has not probed the local service since the last change."))
	}

	segments = append(segments, b.connectorSegment("Network agent"))
	segments = append(segments, b.segment(core.SegmentProviderEdge, "Membership of "+network,
		b.connectorBackedStatus(), "this machine joins the network while the agent runs"))
	segments = append(segments, b.segment(core.SegmentProtection, "Reachable by network members",
		b.connectorBackedStatus(), "only members of "+network+" can reach this address"))

	addressStatus, addressDetail := segmentUnknown, ""
	switch {
	case b.closed:
		addressStatus, addressDetail = segmentNotOpened, "the connection is closed"
	case b.rt != nil && b.rt.Endpoint.PrivateAddress != "":
		addressStatus, addressDetail = segmentOK, b.rt.Endpoint.PrivateAddress
	case b.rt != nil && b.rt.State == core.RuntimeError:
		addressStatus, addressDetail = segmentFailed, "no private address was established"
	}
	segments = append(segments, b.segment(core.SegmentEndpoint, "Private address", addressStatus, addressDetail))

	return segments
}

// joinHostPort formats a remote target, tolerating an unset host or port rather
// than emitting a dangling separator.
func joinHostPort(host string, port int) string {
	switch {
	case host == "" && port == 0:
		return "the remote endpoint"
	case host == "":
		return fmt.Sprintf("port %d", port)
	case port == 0:
		return host
	case strings.Contains(host, ":"):
		return fmt.Sprintf("[%s]:%d", host, port)
	default:
		return fmt.Sprintf("%s:%d", host, port)
	}
}

// resourceStatus reports a provider resource segment from the durable record of
// what Portico created, including resources an authoritative lookup found had
// been removed outside Portico.
func resourceStatus(resources []core.ProviderResource, kind core.ResourceType, closed bool) string {
	for _, r := range resources {
		if r.Type != kind {
			continue
		}
		switch r.Lifecycle {
		case core.LifecycleExternallyRemoved:
			return segmentFailed
		case core.LifecycleRemoved, core.LifecycleRemovalPending:
			return segmentNotOpened
		case core.LifecycleRemovalFailed, core.LifecycleOrphaned:
			return segmentDegraded
		}
		return segmentOK
	}
	if closed {
		return segmentNotOpened
	}
	return segmentUnknown
}

func resourceDetail(resources []core.ProviderResource, kind core.ResourceType) string {
	for _, r := range resources {
		if r.Type == kind {
			if r.Lifecycle == core.LifecycleExternallyRemoved {
				return fmt.Sprintf("%s was deleted outside Portico", r.ExternalID)
			}
			return r.ExternalID
		}
	}
	return ""
}
