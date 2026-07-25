package controller

import (
	"context"
	"fmt"
	"sort"

	"github.com/paoloanzn/portico/internal/core"
	"github.com/paoloanzn/portico/internal/provider"
)

// Recommendation contains the recommendation result.
type Recommendation struct {
	Provider core.ProviderID
	Score    int
	Reasons  []string
	Filtered []provider.FilteredProvider
}

// Recommend recommends the best provider for a desired connection.
func (c *Controller) Recommend(ctx context.Context, profile *core.ConnectionProfile, accountIDs []core.ProviderAccountID) (*Recommendation, error) {
	snapshots := c.registry.Snapshot()
	if len(snapshots) == 0 {
		return nil, fmt.Errorf("no providers available")
	}

	var filtered []provider.FilteredProvider
	var candidates []candidate

	for _, prov := range snapshots {
		caps := prov.Capabilities

		// Hard filter: exposure mode
		switch profile.Exposure.Mode {
		case core.ExposureTemporary:
			if !caps.TemporaryAddresses.Supported {
				filtered = append(filtered, provider.FilteredProvider{Provider: prov.ID, Reason: "does not support temporary addresses"})
				continue
			}
		case core.ExposurePermanent:
			if !caps.CustomHostnames.Supported {
				filtered = append(filtered, provider.FilteredProvider{Provider: prov.ID, Reason: "does not support custom hostnames"})
				continue
			}
		case core.ExposurePrivate:
			if !caps.PrivateExposure.Supported {
				filtered = append(filtered, provider.FilteredProvider{Provider: prov.ID, Reason: "does not support private exposure"})
				continue
			}
		}

		// Hard filter: protection
		if profile.Protection.Kind != core.ProtectionNone {
			hasProtection := false
			for _, pc := range caps.BuiltInProtection {
				if pc.Kind == profile.Protection.Kind && pc.Supported {
					hasProtection = true
					break
				}
			}
			if !hasProtection {
				filtered = append(filtered, provider.FilteredProvider{Provider: prov.ID, Reason: fmt.Sprintf("does not support protection %s", profile.Protection.Kind)})
				continue
			}
		}

		// Hard filter: MCP transport
		if profile.Source.Kind == core.SourceMCP && profile.Source.MCP != nil {
			transport := profile.Source.MCP.Transport
			if profile.Exposure.Mode == core.ExposureTemporary && transport == core.MCPTransportSSE {
				filtered = append(filtered, provider.FilteredProvider{Provider: prov.ID, Reason: "SSE not supported with Quick Tunnel"})
				continue
			}
		}

		// Score the provider
		score := 0
		reasons := []string{}

		if prov.Authenticated {
			score += 10
			reasons = append(reasons, "authenticated")
		}

		// Prefer matching account
		for _, acc := range prov.Accounts {
			for _, reqAcc := range accountIDs {
				if acc.ID == reqAcc {
					score += 5
					reasons = append(reasons, "matching account")
					break
				}
			}
		}

		// Prefer stable capabilities
		if profile.Exposure.Mode == core.ExposureTemporary && caps.TemporaryAddresses.Stability == core.StabilityStable {
			score += 3
			reasons = append(reasons, "stable temporary addresses")
		}

		candidates = append(candidates, candidate{prov, score, reasons})
	}

	// Sort by score (desc), then by ID (asc) for deterministic tie-breaking
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return string(candidates[i].prov.ID) < string(candidates[j].prov.ID)
	})

	if len(candidates) == 0 {
		return &Recommendation{Filtered: filtered}, fmt.Errorf("no provider meets requirements")
	}

	best := candidates[0]
	return &Recommendation{
		Provider: best.prov.ID,
		Score:    best.score,
		Reasons:  best.reasons,
		Filtered: filtered,
	}, nil
}

type candidate struct {
	prov    provider.ProviderSnapshot
	score   int
	reasons []string
}
