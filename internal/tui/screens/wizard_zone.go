package screens

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// WizardZoneMsg carries the DNS zones a Cloudflare account's stored credential
// can see, fetched when the hostname step opens for a permanent connection.
type WizardZoneMsg struct {
	WizardID   WizardID
	Generation WizardGeneration
	Zones      []ipc.ZoneDTO
	Err        error
}

// zoneCmd lists the zones the selected Cloudflare account can serve.
func (m *WizardModel) zoneCmd() tea.Cmd {
	client, ok := m.client.(ZoneLister)
	if !ok {
		return nil
	}
	provider, account := m.state.Provider, m.state.AccountID
	if provider == "" || account == "" {
		return nil
	}
	ctx := m.ctx
	wizID, gen := m.id, m.generation
	m.zonesPending = true
	m.zonesErr = nil
	return func() tea.Msg {
		reply := WizardZoneMsg{WizardID: wizID, Generation: gen}
		zCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		resp, err := client.ListProviderAccountZones(zCtx, provider, account)
		if err != nil {
			reply.Err = err
			return reply
		}
		if resp != nil {
			reply.Zones = resp.Zones
		}
		return reply
	}
}

// HandleZones applies a zone-list result from the running wizard.
func (m *WizardModel) HandleZones(msg WizardZoneMsg) {
	if msg.WizardID != m.id || msg.Generation != m.generation {
		return
	}
	m.zonesPending = false
	if msg.Err != nil {
		// nil plus a set zonesErr is "loading failed"; the hostname step must
		// not present a failure as "an account with no zones".
		m.zonesErr = msg.Err
		m.zones = nil
		m.zoneSelected = nil
		return
	}
	m.zonesErr = nil
	m.zones = msg.Zones
	m.selected = 0
}

// ZoneLister is the zone-listing half of the wizard's client.
type ZoneLister interface {
	ListProviderAccountZones(ctx context.Context, providerID, accountID string) (
		*ipc.ListProviderAccountZonesResponse, error)
}

// zoneForRow maps a chosen `<connection>.<zone>` row back to the zone it names,
// empty when the row does not name a real zone (the manual row).
func (m *WizardModel) zoneForRow(row string) *ipc.ZoneDTO {
	prefix := m.hostnamePrefix() + "."
	if !strings.HasPrefix(row, prefix) {
		return nil
	}
	suffix := strings.TrimPrefix(row, prefix)
	for i := range m.zones {
		if m.zones[i].Name == suffix {
			z := m.zones[i]
			return &z
		}
	}
	return nil
}

// hostnamePrefix returns the `<connection>` half of a proposed hostname.
func (m *WizardModel) hostnamePrefix() string {
	if m.state.Name != "" {
		return m.state.Name
	}
	return "app"
}