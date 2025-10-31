package pdns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	}
}

type createZoneRequest struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"` // "Native" or "Master"
	Nameservers []string `json:"nameservers"`
}

// CreateZone creates an authoritative zone if it does not exist.
func (c *Client) CreateZone(ctx context.Context, zone string, nameservers []string) error {
	// PDNS expects absolute nameserver hostnames (trailing dot)
	nsAbs := make([]string, 0, len(nameservers))
	for _, ns := range nameservers {
		if ns == "" {
			continue
		}
		if ns[len(ns)-1] != '.' {
			ns += "."
		}
		nsAbs = append(nsAbs, ns)
	}
	payload := createZoneRequest{
		Name:        zone + ".",
		Kind:        "Native",
		Nameservers: nsAbs,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/servers/localhost/zones", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		return nil // already exists
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("pdns create zone failed: status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) GetZone(ctx context.Context, zone string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/servers/localhost/zones/"+zone+".", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("pdns get zone failed: status %d", resp.StatusCode)
	}
	var zoneResponse struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&zoneResponse); err != nil {
		return "", err
	}
	return zoneResponse.Name, nil
}

// in package pdns (same file as CreateZone/GetZone)
func (c *Client) DeleteZone(ctx context.Context, zone string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.BaseURL+"/api/v1/servers/localhost/zones/"+zone+".", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		// drain is optional for DELETE (usually no body), but Close error must be handled
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusNotFound {
		return nil // already gone
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("pdns delete zone failed: status %d", resp.StatusCode)
	}
	return nil
}

// GetZoneRRSets fetches all rrsets for a zone and returns them.
func (c *Client) GetZoneRRSets(ctx context.Context, zone string) ([]zoneRRset, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/servers/localhost/zones/"+zone+".", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		// Zone not found yet; treat as empty rrsets for callers that already guard for zone readiness
		return []zoneRRset{}, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("pdns get zone rrsets failed: status %d", resp.StatusCode)
	}
	var zr zoneResponse
	if err := json.NewDecoder(resp.Body).Decode(&zr); err != nil {
		return nil, err
	}
	return zr.RRSets, nil
}

type rrsetRecord struct {
	Content  string `json:"content"`
	Disabled bool   `json:"disabled"`
}

type rrset struct {
	Name       string        `json:"name"`
	Type       string        `json:"type"`
	TTL        int           `json:"ttl"`
	ChangeType string        `json:"changetype"`
	Records    []rrsetRecord `json:"records"`
}

type patchZoneRequest struct {
	RRSets []rrset `json:"rrsets"`
}

// Structures for GET zone response parsing
type zoneResponse struct {
	Name   string      `json:"name"`
	RRSets []zoneRRset `json:"rrsets"`
}

type zoneRRset struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	TTL     int               `json:"ttl"`
	Records []zoneRRsetRecord `json:"records"`
}

type zoneRRsetRecord struct {
	Content  string `json:"content"`
	Disabled bool   `json:"disabled"`
}

// UpdateSOA replaces the SOA rrset for the given zone with provided MNAME and RNAME.
func (c *Client) UpdateSOA(ctx context.Context, zone, mname, rname string) error {
	serial := time.Now().Format("20060102") + "01"
	soaContent := fmt.Sprintf("%s %s %s 10800 3600 604800 3600", mname, rname, serial)
	payload := patchZoneRequest{
		RRSets: []rrset{{
			Name:       zone + ".",
			Type:       "SOA",
			TTL:        3600,
			ChangeType: "REPLACE",
			Records:    []rrsetRecord{{Content: soaContent, Disabled: false}},
		}},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.BaseURL+"/api/v1/servers/localhost/zones/"+zone+".", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("pdns update soa failed: status %d", resp.StatusCode)
	}
	return nil
}

// ApplyRecordSetAuthoritative ensures rrsets for the given record type match exactly the owners provided
// in rs.Spec.Records: it REPLACEs provided owners and DELETEs any extra owners of the same type in PDNS.
func (c *Client) ApplyRecordSetAuthoritative(ctx context.Context, zone string, rs dnsv1alpha1.DNSRecordSet) error {
	desired := buildRRSets(zone, rs)
	// Build a set of desired owners for quick lookup
	desiredOwners := map[string]struct{}{}
	for _, rr := range desired {
		// Only target the specific type for this recordset
		if rr.Type == string(rs.Spec.RecordType) {
			desiredOwners[rr.Name] = struct{}{}
		}
	}

	// Fetch existing rrsets and find owners of this type to delete if not in desired
	existing, err := c.GetZoneRRSets(ctx, zone)
	if err != nil {
		return err
	}
	deletes := make([]rrset, 0)
	for _, ex := range existing {
		if ex.Type != string(rs.Spec.RecordType) {
			continue
		}
		// Normalize name to absolute form
		name := ex.Name
		if _, ok := desiredOwners[name]; !ok {
			deletes = append(deletes, rrset{
				Name:       name,
				Type:       ex.Type,
				TTL:        0,
				ChangeType: "DELETE",
				Records:    []rrsetRecord{},
			})
		}
	}

	payload := patchZoneRequest{RRSets: append(desired, deletes...)}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.BaseURL+"/api/v1/servers/localhost/zones/"+zone+".", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("pdns apply authoritative failed: status %d", resp.StatusCode)
	}
	return nil
}

func buildRRSets(zone string, rs dnsv1alpha1.DNSRecordSet) []rrset {
	out := make([]rrset, 0, len(rs.Spec.Records))
	for _, rec := range rs.Spec.Records {
		ttl := 300
		if rec.TTL != nil {
			ttl = int(*rec.TTL)
		}
		name := qualifyOwner(rec.Name, zone)
		switch rs.Spec.RecordType {
		case dnsv1alpha1.RRTypeA:
			values := rec.Raw
			if rec.A != nil {
				values = rec.A.Content
			}
			out = append(out, makeSimpleRRSet(name, "A", ttl, values))
		case dnsv1alpha1.RRTypeAAAA:
			values := rec.Raw
			if rec.AAAA != nil {
				values = rec.AAAA.Content
			}
			out = append(out, makeSimpleRRSet(name, "AAAA", ttl, values))
		case dnsv1alpha1.RRTypeCNAME:
			target := ""
			if rec.CNAME != nil {
				target = rec.CNAME.Content
			} else if len(rec.Raw) > 0 {
				target = rec.Raw[0]
			}
			if target != "" && target[len(target)-1] != '.' {
				target += "."
			}
			out = append(out, rrset{
				Name:       name,
				Type:       "CNAME",
				TTL:        ttl,
				ChangeType: "REPLACE",
				Records:    []rrsetRecord{{Content: target, Disabled: false}},
			})
		case dnsv1alpha1.RRTypeTXT:
			values := rec.Raw
			if rec.TXT != nil {
				values = rec.TXT.Content
			}
			quoted := make([]string, 0, len(values))
			for _, v := range values {
				quoted = append(quoted, quoteIfNeeded(v))
			}
			out = append(out, makeSimpleRRSet(name, "TXT", ttl, quoted))
		case dnsv1alpha1.RRTypeMX:
			lines := make([]string, 0, len(rec.MX))
			for _, mx := range rec.MX {
				exch := mx.Exchange
				if exch != "" && exch[len(exch)-1] != '.' {
					exch += "."
				}
				lines = append(lines, fmt.Sprintf("%d %s", mx.Preference, exch))
			}
			if len(lines) == 0 && len(rec.Raw) > 0 {
				lines = append(lines, rec.Raw...)
			}
			out = append(out, makeSimpleRRSet(name, "MX", ttl, lines))
		case dnsv1alpha1.RRTypeSRV:
			lines := make([]string, 0, len(rec.SRV))
			for _, s := range rec.SRV {
				tgt := s.Target
				if tgt != "" && tgt[len(tgt)-1] != '.' {
					tgt += "."
				}
				lines = append(lines, fmt.Sprintf("%d %d %d %s", s.Priority, s.Weight, s.Port, tgt))
			}
			if len(lines) == 0 && len(rec.Raw) > 0 {
				lines = append(lines, rec.Raw...)
			}
			out = append(out, makeSimpleRRSet(name, "SRV", ttl, lines))
		case dnsv1alpha1.RRTypeCAA:
			lines := make([]string, 0, len(rec.CAA))
			for _, c := range rec.CAA {
				lines = append(lines, fmt.Sprintf("%d %s %s", c.Flag, c.Tag, quoteIfNeeded(c.Value)))
			}
			if len(lines) == 0 && len(rec.Raw) > 0 {
				lines = append(lines, rec.Raw...)
			}
			out = append(out, makeSimpleRRSet(name, "CAA", ttl, lines))
		case dnsv1alpha1.RRTypeNS:
			values := rec.Raw
			if len(values) == 0 && rec.A != nil {
				values = rec.A.Content
			}
			fq := make([]string, 0, len(values))
			for _, v := range values {
				if v != "" && v[len(v)-1] != '.' {
					v += "."
				}
				fq = append(fq, v)
			}
			out = append(out, makeSimpleRRSet(name, "NS", ttl, fq))
		case dnsv1alpha1.RRTypeSOA:
			if rec.SOA != nil {
				mname := qualifyIfNeeded(rec.SOA.MName)
				rname := qualifyIfNeeded(rec.SOA.RName)
				serial := fmt.Sprintf("%s01", time.Now().Format("20060102"))
				if rec.SOA.Serial != 0 {
					serial = fmt.Sprintf("%d", rec.SOA.Serial)
				}
				refresh := uint32(10800)
				retry := uint32(3600)
				expire := uint32(604800)
				minimum := uint32(3600)
				if rec.SOA.Refresh != 0 {
					refresh = rec.SOA.Refresh
				}
				if rec.SOA.Retry != 0 {
					retry = rec.SOA.Retry
				}
				if rec.SOA.Expire != 0 {
					expire = rec.SOA.Expire
				}
				if rec.SOA.TTL != 0 {
					minimum = rec.SOA.TTL
				}
				line := fmt.Sprintf("%s %s %s %d %d %d %d", mname, rname, serial, refresh, retry, expire, minimum)
				out = append(out, makeSimpleRRSet(name, "SOA", ttl, []string{line}))
			} else if len(rec.Raw) > 0 {
				out = append(out, makeSimpleRRSet(name, "SOA", ttl, rec.Raw))
			}
		case dnsv1alpha1.RRTypePTR:
			out = append(out, makeSimpleRRSet(name, "PTR", ttl, rec.Raw))
		case dnsv1alpha1.RRTypeTLSA:
			lines := make([]string, 0, len(rec.TLSA))
			for _, t := range rec.TLSA {
				lines = append(lines, fmt.Sprintf("%d %d %d %s", t.Usage, t.Selector, t.MatchingType, t.CertData))
			}
			if len(lines) == 0 && len(rec.Raw) > 0 {
				lines = append(lines, rec.Raw...)
			}
			out = append(out, makeSimpleRRSet(name, "TLSA", ttl, lines))
		case dnsv1alpha1.RRTypeHTTPS:
			lines := make([]string, 0, len(rec.HTTPS))
			for _, h := range rec.HTTPS {
				params := ""
				for k, v := range h.Params {
					if params != "" {
						params += " "
					}
					params += fmt.Sprintf("%s=%s", k, quoteIfNeeded(v))
				}
				lines = append(lines, fmt.Sprintf("%d %s %s", h.Priority, qualifyIfNeeded(h.Target), params))
			}
			if len(lines) == 0 && len(rec.Raw) > 0 {
				lines = append(lines, rec.Raw...)
			}
			out = append(out, makeSimpleRRSet(name, "HTTPS", ttl, lines))
		case dnsv1alpha1.RRTypeSVCB:
			lines := make([]string, 0, len(rec.SVCB))
			for _, h := range rec.SVCB {
				params := ""
				for k, v := range h.Params {
					if params != "" {
						params += " "
					}
					params += fmt.Sprintf("%s=%s", k, quoteIfNeeded(v))
				}
				lines = append(lines, fmt.Sprintf("%d %s %s", h.Priority, qualifyIfNeeded(h.Target), params))
			}
			if len(lines) == 0 && len(rec.Raw) > 0 {
				lines = append(lines, rec.Raw...)
			}
			out = append(out, makeSimpleRRSet(name, "SVCB", ttl, lines))
		}
	}
	return out
}

func makeSimpleRRSet(name, typ string, ttl int, values []string) rrset {
	recs := make([]rrsetRecord, 0, len(values))
	for _, v := range values {
		recs = append(recs, rrsetRecord{Content: v, Disabled: false})
	}
	return rrset{
		Name:       name,
		Type:       typ,
		TTL:        ttl,
		ChangeType: "REPLACE",
		Records:    recs,
	}
}

func qualifyOwner(owner, zone string) string {
	if owner == "@" || owner == "" {
		return zone + "."
	}
	if owner[len(owner)-1] == '.' {
		return owner
	}
	return owner + "." + zone + "."
}

func qualifyIfNeeded(target string) string {
	if target == "" {
		return target
	}
	if target[len(target)-1] == '.' {
		return target
	}
	return target + "."
}

func quoteIfNeeded(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"') {
		return s
	}
	return fmt.Sprintf("\"%s\"", s)
}

// NewFromEnv constructs a PowerDNS API client using environment variables.
//
// Required/optional env vars:
// - PDNS_API_URL: base URL for the HTTP API (default: http://127.0.0.1:8081)
// - PDNS_API_KEY: API key (required)
func NewFromEnv() (*Client, error) {
	url := getenvDefault("PDNS_API_URL", "http://127.0.0.1:8081")
	apiKey := os.Getenv("PDNS_API_KEY")
	if apiKey == "" {
		if path := os.Getenv("PDNS_API_KEY_FILE"); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read PDNS_API_KEY_FILE: %w", err)
			}
			apiKey = string(bytes.TrimSpace(data))
		}
	}
	if apiKey == "" {
		return nil, fmt.Errorf("PDNS_API_KEY or PDNS_API_KEY_FILE is required")
	}
	return NewClient(url, apiKey), nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
