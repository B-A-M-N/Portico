package dns

import (
	"context"
	"errors"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go"
)

// Manager manages Cloudflare DNS records.
type Manager interface {
	CreateCNAME(ctx context.Context, zoneID, hostname, tunnelID string) (recordID string, err error)
	UpdateCNAME(ctx context.Context, zoneID, recordID, hostname, tunnelID string) error
	GetRecord(ctx context.Context, zoneID, recordID string) (*RecordState, error)
	DeleteRecord(ctx context.Context, zoneID, recordID string) error
}

// RecordState represents the observed state of a DNS record.
type RecordState struct {
	ID      string
	Name    string
	Type    string
	Content string
	Proxied bool
}

// APIManager implements Manager using the Cloudflare API.
type APIManager struct {
	client *cf.API
}

// NewAPIManager creates a new DNS manager.
func NewAPIManager(client *cf.API) *APIManager {
	return &APIManager{client: client}
}

// CreateCNAME creates a proxied CNAME record pointing to the tunnel.
func (m *APIManager) CreateCNAME(ctx context.Context, zoneID, hostname, tunnelID string) (string, error) {
	rc := cf.ZoneIdentifier(zoneID)

	proxied := true
	record, err := m.client.CreateDNSRecord(ctx, rc, cf.CreateDNSRecordParams{
		Type:    "CNAME",
		Name:    hostname,
		Content: fmt.Sprintf("%s.cfargotunnel.com", tunnelID),
		TTL:     1, // Auto TTL.
		Proxied: &proxied,
		Comment: "Created by Portico",
	})
	if err != nil {
		return "", fmt.Errorf("creating CNAME record: %w", err)
	}

	return record.ID, nil
}

// UpdateCNAME retargets one exact managed CNAME record without deleting and
// recreating it. This preserves the durable resource identity and avoids an
// ambiguous intermediate state if the provider call fails.
func (m *APIManager) UpdateCNAME(ctx context.Context, zoneID, recordID, hostname, tunnelID string) error {
	rc := cf.ZoneIdentifier(zoneID)
	proxied := true
	_, err := m.client.UpdateDNSRecord(ctx, rc, cf.UpdateDNSRecordParams{
		ID:      recordID,
		Type:    "CNAME",
		Name:    hostname,
		Content: fmt.Sprintf("%s.cfargotunnel.com", tunnelID),
		TTL:     1,
		Proxied: &proxied,
	})
	if err != nil {
		return fmt.Errorf("updating CNAME record: %w", err)
	}
	return nil
}

// GetRecord retrieves the current state of a DNS record.
// Returns nil, nil if the record is not found (deleted externally).
func (m *APIManager) GetRecord(ctx context.Context, zoneID, recordID string) (*RecordState, error) {
	rc := cf.ZoneIdentifier(zoneID)
	record, err := m.client.GetDNSRecord(ctx, rc, recordID)
	if err != nil {
		// errors.As, not a type assertion: the client wraps its errors, so a
		// direct assertion silently fails and a deleted record is reported
		// as a failed lookup instead of an absent one. One of the four places
		// that made this check had already been corrected; the others had not.
		var cfErr *cf.Error
		if errors.As(err, &cfErr) && cfErr.StatusCode == 404 {
			return nil, nil
		}
		return nil, fmt.Errorf("getting DNS record: %w", err)
	}
	return &RecordState{
		ID:      record.ID,
		Name:    record.Name,
		Type:    record.Type,
		Content: record.Content,
		Proxied: *record.Proxied,
	}, nil
}
func (m *APIManager) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	rc := cf.ZoneIdentifier(zoneID)
	if err := m.client.DeleteDNSRecord(ctx, rc, recordID); err != nil {
		return fmt.Errorf("deleting DNS record: %w", err)
	}
	return nil
}
