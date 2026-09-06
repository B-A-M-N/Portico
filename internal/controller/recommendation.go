package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/B-A-M-N/portico/internal/core"
	profilepkg "github.com/B-A-M-N/portico/internal/profile"
	"github.com/B-A-M-N/portico/internal/provider"
)

// RecommendationInput describes what a connection actually needs. The engine
// evaluates providers against these requirements rather than picking whichever
// provider happens to be authenticated first.
type RecommendationInput struct {
	// PreferredProvider owns PreferredAccount. Account identity is the pair, so
	// a preference without its provider is ambiguous.
	PreferredProvider core.ProviderID
	Kind              core.ConnectionKind
	SourceKind        core.SourceKind
	MCPTransport      core.MCPTransport
	ExposureMode      core.ExposureMode
	Protocol          core.Protocol
	ProtectionKind    core.ProtectionKind
	RequestedAddress  string
	PreferredAccount  core.ProviderAccountID
	// ProfileKind is the workload the connection carries ("openai_mcp",
	// "openai_compatible", "web_service"). Empty derives from SourceKind,
	// matching how the create path derives it.
	ProfileKind core.ProfileKind
}

// ProviderEvaluation is the verdict for one provider against the requirements.
//
// An ineligible provider is returned with the reasons it cannot be used rather
// than being dropped, so the caller can explain the absence instead of
// presenting a shorter list with no justification.
type ProviderEvaluation struct {
	ProviderID      core.ProviderID
	DisplayName     string
	AccountID       core.ProviderAccountID
	Eligible        bool
	BlockingReasons []string
	Strengths       []string
	Tradeoffs       []string
	SetupActions    []string
	Score           int
}

// Recommendation is the result of evaluating every catalogued provider.
type Recommendation struct {
	Recommended  *ProviderEvaluation
	Alternatives []ProviderEvaluation
	Ineligible   []ProviderEvaluation
	// Summary explains the outcome in plain language, including the case where
	// nothing is eligible.
	Summary string
}

// Recommend evaluates every catalogued provider against the requirements.
//
// Hard constraints are applied first: a provider that cannot satisfy a stated
// requirement is ineligible regardless of how attractive it otherwise looks.
// Scoring only orders the providers that remain.
func (c *Controller) Recommend(ctx context.Context, input RecommendationInput) (*Recommendation, error) {
	snapshots := c.registry.DiscoverIdentities(ctx)
	if len(snapshots) == 0 {
		return &Recommendation{Summary: "No providers are catalogued."}, nil
	}

	var eligible, ineligible []ProviderEvaluation
	for _, prov := range snapshots {
		eval := evaluateProvider(prov, input, c.profileRegistry)
		if eval.Eligible {
			eligible = append(eligible, eval)
		} else {
			ineligible = append(ineligible, eval)
		}
	}

	// Deterministic ordering: score descending, then provider ID ascending so
	// equal candidates never reorder between calls.
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Score != eligible[j].Score {
			return eligible[i].Score > eligible[j].Score
		}
		return string(eligible[i].ProviderID) < string(eligible[j].ProviderID)
	})
	sort.Slice(ineligible, func(i, j int) bool {
		return string(ineligible[i].ProviderID) < string(ineligible[j].ProviderID)
	})

	result := &Recommendation{Ineligible: ineligible}
	if len(eligible) == 0 {
		result.Summary = summariseNoCandidate(input, ineligible)
		return result, nil
	}

	best := eligible[0]
	result.Recommended = &best
	result.Alternatives = eligible[1:]
	result.Summary = summariseChoice(best, input)
	return result, nil
}

// evaluateProvider applies hard constraints, then scores what survives.
//
// profileRegistry may be nil: callers without one skip the workload-profile
// constraint, exactly as validateProfileTransport does.
func evaluateProvider(prov provider.ProviderSnapshot, input RecommendationInput, reg *profilepkg.Registry) ProviderEvaluation {
	eval := ProviderEvaluation{
		ProviderID:   prov.ID,
		DisplayName:  prov.DisplayName,
		SetupActions: append([]string(nil), prov.SetupActions...),
	}
	if eval.DisplayName == "" {
		eval.DisplayName = string(prov.ID)
	}

	// Constraint: the provider must actually be usable, decided by the one
	// authority rather than by this package's own reading. A provider that
	// cannot carry a connection must not be recommended however well its
	// declared capabilities match — including an unconfigured one, which was
	// previously eligible and could be returned as the best choice for a
	// connection it had no account to open.
	if !prov.Availability.Selectable() {
		eval.BlockingReasons = append(eval.BlockingReasons,
			prov.Availability.UnavailableReason(prov.CapabilityError))
	}

	caps := prov.Capabilities

	// Constraint: connection kind. A provider is asked what it can execute
	// rather than being measured against a hardcoded list — that list said only
	// service exposure was executable, while Portico had been creating and
	// running local port forwards for some time, so the engine refused a kind
	// the rest of the system supported.
	if !caps.Executes(input.Kind) {
		eval.BlockingReasons = append(eval.BlockingReasons,
			fmt.Sprintf("does not support %s connections", input.Kind))
	}

	// Constraint: exposure mode.
	switch input.ExposureMode {
	case core.ExposureTemporary:
		if !caps.TemporaryAddresses.Supported {
			eval.BlockingReasons = append(eval.BlockingReasons, "does not support temporary addresses")
		}
	case core.ExposurePermanent:
		if !caps.CustomHostnames.Supported {
			eval.BlockingReasons = append(eval.BlockingReasons, "does not support permanent custom hostnames")
		}
		if !caps.ManagedDNS.Supported {
			eval.BlockingReasons = append(eval.BlockingReasons, "cannot manage DNS for a permanent hostname")
		}
	case core.ExposurePrivate:
		if !caps.PrivateExposure.Supported {
			eval.BlockingReasons = append(eval.BlockingReasons, "does not support private-only exposure")
		}
	}

	// Constraint: a requested hostname requires custom hostname support even if
	// the mode was not stated explicitly.
	if input.RequestedAddress != "" && !caps.CustomHostnames.Supported {
		eval.BlockingReasons = append(eval.BlockingReasons, "cannot serve a specific hostname")
	}

	// Constraint: protocol.
	if input.Protocol != "" {
		if pc, ok := caps.Protocols[input.Protocol]; !ok || !pc.Supported {
			eval.BlockingReasons = append(eval.BlockingReasons,
				fmt.Sprintf("does not support the %s protocol", input.Protocol))
		}
	}

	// Constraint: workload profile. The create path refuses a transport that
	// cannot carry the connection's profile (validateProfileTransport, surface
	// code PTO-CORE-009). Evaluating the same compatibility here is what keeps
	// the wizard from offering that provider: a refusal the user first meets
	// after answering every question is a refusal shown too late.
	if kind := requiredProfileKind(input); kind != "" && reg != nil {
		if def, ok := reg.Lookup(kind); ok && !def.Compatible(transportCapabilities(caps)) {
			eval.BlockingReasons = append(eval.BlockingReasons,
				profileBlockingReason(kind))
		}
	}

	// Constraint: protection.
	if input.ProtectionKind != "" && input.ProtectionKind != core.ProtectionNone {
		supported := false
		for _, pc := range caps.BuiltInProtection {
			if pc.Kind == input.ProtectionKind && pc.Supported {
				supported = true
				break
			}
		}
		if !supported {
			eval.BlockingReasons = append(eval.BlockingReasons,
				fmt.Sprintf("does not support %s protection", input.ProtectionKind))
		}
	}

	// Constraint: MCP over SSE cannot use a temporary address, because the
	// address changes and an SSE client cannot follow it.
	if input.SourceKind == core.SourceMCP &&
		input.MCPTransport == core.MCPTransportSSE &&
		input.ExposureMode == core.ExposureTemporary {
		eval.BlockingReasons = append(eval.BlockingReasons,
			"SSE transport cannot be used with a temporary address")
	}

	if len(eval.BlockingReasons) > 0 {
		return eval
	}
	eval.Eligible = true

	// Scoring. Only reached by providers that satisfy every hard constraint.
	// Having an account is a strength, not a requirement: a Quick Tunnel and a
	// local port forward are ready and accountless, and describing them as
	// needing account setup was simply false. Whether a provider can be used is
	// already settled by its availability above.
	if prov.Authenticated {
		eval.Score += 10
		eval.Strengths = append(eval.Strengths, "already authenticated")
	}

	// An account preference belongs to the provider that owns it. Account
	// identity is the pair, so matching on the ID alone credited a preference
	// for one provider's "default" to every other provider with an account of
	// the same name.
	if input.PreferredAccount != "" && input.PreferredProvider == prov.ID {
		for _, acc := range prov.Accounts {
			if acc.ID == input.PreferredAccount {
				eval.Score += 5
				eval.AccountID = acc.ID
				eval.Strengths = append(eval.Strengths, "uses the account you selected")
				break
			}
		}
	}
	// Naming an account is a claim that this provider was evaluated with it.
	// With one account that is true. With several, picking the first is
	// deterministic ordering rather than a judgement, and naming it caused the
	// caller to skip asking which one the user wanted.
	if eval.AccountID == "" && len(prov.Accounts) == 1 {
		eval.AccountID = prov.Accounts[0].ID
	}

	switch input.ExposureMode {
	case core.ExposureTemporary:
		if caps.TemporaryAddresses.Stability == core.StabilityStable {
			eval.Score += 3
			eval.Strengths = append(eval.Strengths, "temporary addresses are a stable, supported feature")
		}
		eval.Tradeoffs = append(eval.Tradeoffs, "the address changes each time the connection is opened")
	case core.ExposurePermanent:
		if caps.CustomHostnames.Stability == core.StabilityStable {
			eval.Score += 3
			eval.Strengths = append(eval.Strengths, "custom hostnames are a stable, supported feature")
		}
	}

	if input.ProtectionKind == "" || input.ProtectionKind == core.ProtectionNone {
		if input.ExposureMode == core.ExposureTemporary || input.ExposureMode == core.ExposurePermanent {
			eval.Tradeoffs = append(eval.Tradeoffs, "anyone with the address can reach the service")
		}
	}

	if caps.Telemetry.Supported {
		eval.Score++
		eval.Strengths = append(eval.Strengths, "reports traffic telemetry")
	}

	return eval
}

func summariseChoice(best ProviderEvaluation, input RecommendationInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is the best match", best.DisplayName)
	switch input.ExposureMode {
	case core.ExposureTemporary:
		b.WriteString(" for a temporary public address")
	case core.ExposurePermanent:
		b.WriteString(" for a permanent public address")
	case core.ExposurePrivate:
		b.WriteString(" for private access")
	}
	b.WriteString(".")
	if len(best.Strengths) > 0 {
		fmt.Fprintf(&b, " It %s.", strings.Join(best.Strengths, ", and "))
	}
	return b.String()
}

// summariseNoCandidate explains an empty result. Returning no recommendation is
// a legitimate outcome and must be stated as such, rather than falling back to
// a provider that cannot meet the requirements.
func summariseNoCandidate(input RecommendationInput, ineligible []ProviderEvaluation) string {
	var b strings.Builder
	b.WriteString("No provider can meet these requirements")
	var needs []string
	if input.ExposureMode != "" {
		needs = append(needs, string(input.ExposureMode))
	}
	if input.Protocol != "" {
		needs = append(needs, string(input.Protocol))
	}
	if input.ProtectionKind != "" && input.ProtectionKind != core.ProtectionNone {
		needs = append(needs, string(input.ProtectionKind)+" protection")
	}
	if len(needs) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(needs, ", "))
	}
	b.WriteString(".")
	for _, e := range ineligible {
		if len(e.BlockingReasons) > 0 {
			fmt.Fprintf(&b, " %s: %s.", e.DisplayName, strings.Join(e.BlockingReasons, "; "))
		}
	}
	return b.String()
}

// requiredProfileKind names the workload profile a recommendation input
// implies. It mirrors derivedProfileKind for the pre-create inputs the
// wizard evaluates: an MCP source is carried by the openai_mcp profile, and
// every other input implies the web-service default. An explicit profile on
// the request wins when one is given. The empty result means no profile
// constraint applies.
func requiredProfileKind(input RecommendationInput) profilepkg.ProfileKind {
	if input.ProfileKind != "" {
		return profilepkg.ProfileKind(input.ProfileKind)
	}
	if input.SourceKind == core.SourceMCP {
		return profilepkg.ProfileOpenAIMCP
	}
	return ""
}

// profileBlockingReason states the refusal in the user's own terms.
func profileBlockingReason(kind profilepkg.ProfileKind) string {
	if kind == profilepkg.ProfileOpenAIMCP {
		return "cannot carry an MCP workload, which needs streaming"
	}
	return fmt.Sprintf("cannot carry the %s workload", kind)
}
