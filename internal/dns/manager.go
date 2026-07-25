package dns

import (
	"context"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go"
)

// Manager manages Cloudflare DNS records.
type Manager interface {
	CreateCNAME(ctx context.Context, zoneID, hostname, tunnelID string) (recordID string, err error)
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
		Comment: "Created by flare-cli",
	})
	if err != nil {
		return "", fmt.Errorf("creating CNAME record: %w", err)
	}

	return record.ID, nil
}

// GetRecord retrieves the current state of a DNS record.
// Returns nil, nil if the record is not found (deleted externally).
func (m *APIManager) GetRecord(ctx context.Context, zoneID, recordID string) (*RecordState, error) {
	rc := cf.ZoneIdentifier(zoneID)
	record, err := m.client.GetDNSRecord(ctx, rc, recordID)
	if err != nil {
		if cfErr, ok := err.(*cf.Error); ok && cfErr.StatusCode == 404 {
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
