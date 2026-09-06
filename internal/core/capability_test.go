package core

import "testing"

func TestCapabilityConstraintMCPTransportDoesNotMatchNonMCPSource(t *testing.T) {
	constraint := CapabilityConstraint{
		Code:          "quick_tunnel_no_sse",
		MCPTransports: []MCPTransport{MCPTransportSSE},
		ExposureModes: []ExposureMode{ExposureTemporary},
		Message:       "SSE is not supported",
	}

	ordinary := &ConnectionProfile{
		Spec: ConnectionSpec{ServiceExposure: &ServiceExposureSpec{
			Source:   SourceSpec{Kind: SourceDirectory},
			Exposure: ExposureSpec{Mode: ExposureTemporary},
		}},
	}
	if violations := CheckConstraints([]CapabilityConstraint{constraint}, ordinary); len(violations) != 0 {
		t.Fatalf("ordinary source matched MCP-only constraint: %#v", violations)
	}

	nonSSE := ordinary.DeepCopy()
	nonSSE.Spec.ServiceExposure.Source = SourceSpec{
		Kind: SourceMCP,
		MCP:  &MCPServiceSpec{Transport: MCPTransportHTTP},
	}
	if violations := CheckConstraints([]CapabilityConstraint{constraint}, nonSSE); len(violations) != 0 {
		t.Fatalf("non-SSE MCP source matched SSE-only constraint: %#v", violations)
	}

	sse := ordinary.DeepCopy()
	sse.Spec.ServiceExposure.Source = SourceSpec{
		Kind: SourceMCP,
		MCP:  &MCPServiceSpec{Transport: MCPTransportSSE},
	}
	if violations := CheckConstraints([]CapabilityConstraint{constraint}, sse); len(violations) != 1 {
		t.Fatalf("SSE MCP source violations = %#v, want one", violations)
	}
}
