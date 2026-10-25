package store

import (
	"context"
	"time"
)

// DNSRecord is a row of dns_records: a record stackr made, or adopted, on a
// managed domain. Name is relative to Zone ("@" the apex). DNSDomainID and DomainID
// (the tile domain that wants it) go nil when their row is deleted.
type DNSRecord struct {
	ID          string  `db:"id" json:"id"`
	DNSDomainID *string `db:"dns_domain_id" json:"dns_domain_id"`
	Zone        string  `db:"zone" json:"zone"`
	Name        string  `db:"name" json:"name"`
	Type        string  `db:"type" json:"type"`
	Value       string  `db:"value" json:"value"`
	TTL         int     `db:"ttl" json:"ttl"`
	DomainID    *string `db:"domain_id" json:"domain_id"`
	// Adopted: the record was already in the zone and stackr took it over.
	// It is repointed when its host's ingress moves, never deleted.
	Adopted   bool      `db:"adopted" json:"adopted"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type DNSRecordStore interface {
	Create(ctx context.Context, r DNSRecord) error
	Update(ctx context.Context, r DNSRecord) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]DNSRecord, error)
}

var dnsRecordsT = newTable[DNSRecord]("dns_records", nil)

type dnsRecords struct{ crud[DNSRecord] }

func (s dnsRecords) List(ctx context.Context) ([]DNSRecord, error) { return s.many(ctx, "1 = 1") }
