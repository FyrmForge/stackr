package store

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"time"
)

// DNSRecord kinds: what a row of dns_records was written for.
const (
	RecordApp    = "app"    // an A/AAAA for a tile hostname (DomainID; nil = the hostname is gone)
	RecordName   = "name"   // the panel's or a node's own address
	RecordCustom = "custom" // a declared record: an app hostname's (DomainID) or the domain's (no DomainID)
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
	Kind      string    `db:"kind" json:"kind"`
	// PutBackAt is when stackr last put a custom record back after it
	// changed outside stackr.
	PutBackAt *time.Time `db:"put_back_at" json:"put_back_at"`
	// HandedBackAt is when a narrowing gave the record back to the user:
	// List never returns such a row, ListHandedBack does.
	HandedBackAt *time.Time `db:"handed_back_at" json:"handed_back_at"`
}

type DNSRecordStore interface {
	Create(ctx context.Context, r DNSRecord) error
	Update(ctx context.Context, r DNSRecord) error
	Delete(ctx context.Context, id string) error
	// List is the records stackr manages: never a handed-back one.
	List(ctx context.Context) ([]DNSRecord, error)
	ListHandedBack(ctx context.Context) ([]DNSRecord, error)
}

var dnsRecordsT = newTable[DNSRecord]("dns_records", nil)

type dnsRecords struct{ crud[DNSRecord] }

func (s dnsRecords) List(ctx context.Context) ([]DNSRecord, error) {
	return s.many(ctx, "handed_back_at IS NULL")
}

func (s dnsRecords) ListHandedBack(ctx context.Context) ([]DNSRecord, error) {
	return s.many(ctx, "handed_back_at IS NOT NULL")
}

// CustomRecord is one declared DNS record, kept as JSON on its owner:
// domains.records (Name relative to the host, "" the host itself) or
// dns_domains.records (Name relative to the domain, "@" the apex).
type CustomRecord struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// CustomRecords is a JSON list column.
type CustomRecords []CustomRecord

func (l CustomRecords) Value() (driver.Value, error) {
	if l == nil {
		return "[]", nil
	}
	b, err := json.Marshal([]CustomRecord(l))
	return string(b), err
}

func (l *CustomRecords) Scan(src any) error {
	if err := scanJSON("CustomRecords", src, (*[]CustomRecord)(l)); err != nil {
		return err
	}
	if len(*l) == 0 {
		*l = nil // none reads back as written
	}
	return nil
}
