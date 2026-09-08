package ipc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// zonesHandler answers the zone-listing route.
type zonesHandler struct {
	nullHandler
	gotProvider, gotAccount string
	zones                  []ZoneDTO
	err                    error
}

func (h *zonesHandler) HandleListProviderAccountZones(providerID, accountID string) (*ListProviderAccountZonesResponse, error) {
	h.gotProvider, h.gotAccount = providerID, accountID
	if h.err != nil {
		return nil, h.err
	}
	return &ListProviderAccountZonesResponse{ProviderID: providerID, AccountID: accountID, Zones: h.zones}, nil
}

// TestListProviderAccountZonesRouteDeliversZones pins finding-7's transport:
// the GET /v1/providers/{id}/accounts/{accountID}/zones route returns every
// zone the account can see, carrying the handler's zones over the real HTTP
// server path.
func TestListProviderAccountZonesRouteDeliversZones(t *testing.T) {
	h := &zonesHandler{
		zones: []ZoneDTO{
			{ID: "zone-default", Name: "example.com"},
			{ID: "zone-b", Name: "other.net"},
		},
	}
	s := serverWith(t, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/providers/cloudflare/accounts/acct-1/zones", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if h.gotProvider != "cloudflare" || h.gotAccount != "acct-1" {
		t.Fatalf("handler received provider %q account %q", h.gotProvider, h.gotAccount)
	}
	var resp ListProviderAccountZonesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Zones) != 2 {
		t.Fatalf("zones = %d, want 2", len(resp.Zones))
	}
	if resp.Zones[0].Name != "example.com" || resp.Zones[1].Name != "other.net" {
		t.Fatalf("zones = %+v, want example.com and other.net", resp.Zones)
	}
}

// TestListProviderAccountZonesClientReturnsZones pins the client half of the
// transport: Client.ListProviderAccountZones reaches the handler through a real
// unix socket and returns the decoded zones.
func TestListProviderAccountZonesClientReturnsZones(t *testing.T) {
	st := openTestStore(t)
	h := &zonesHandler{
		zones: []ZoneDTO{{ID: "z1", Name: "zone1.net"}},
	}
	server := setupEventServer(t, st, h)
	client := NewClient(server.socketPath)
	resp, err := client.ListProviderAccountZones(context.Background(), "cloudflare", "acct-1")
	if err != nil {
		t.Fatalf("ListProviderAccountZones: %v", err)
	}
	if h.gotProvider != "cloudflare" || h.gotAccount != "acct-1" {
		t.Fatalf("handler received provider %q account %q", h.gotProvider, h.gotAccount)
	}
	if len(resp.Zones) != 1 || resp.Zones[0].ID != "z1" {
		t.Fatalf("zones = %+v, want z1", resp.Zones)
	}
}