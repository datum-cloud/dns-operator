package pdns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
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

type pdnsAPIError struct {
	Status int
	Body   string
}

func (e *pdnsAPIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("status %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("error: status %d", e.Status)
}

func readRespBody(resp *http.Response, max int64) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	// don't blow up logs; cap at e.g. 16KB
	if max <= 0 {
		max = 16 << 10 // 16 KiB
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, max))
	return strings.TrimSpace(string(b))
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

// ApplyRecordSetAuthoritative ensures rrsets for the given record type match exactly the owners provided
// in rs.Spec.Records: it REPLACEs provided owners and DELETEs any extra owners of the same type in PDNS.
func (c *Client) ApplyRecordSetAuthoritative(ctx context.Context, zone string, rs dnsv1alpha1.DNSRecordSet) error {
	// Build desired rrsets for this zone+type
	desiredAll := buildRRSets(zone, rs)

	// Filter only the target type (defensive) and normalize empty-record rrsets:
	// - If an rrset has 0 records, PDNS will reject a REPLACE. Convert it to a DELETE instead.
	desired := make([]rrset, 0, len(desiredAll))
	desiredOwners := make(map[string]struct{}, len(desiredAll))
	for _, rr := range desiredAll {
		if rr.Type != string(rs.Spec.RecordType) {
			continue
		}
		if len(rr.Records) == 0 {
			rr.ChangeType = "DELETE"
		} else {
			rr.ChangeType = "REPLACE"
		}
		desired = append(desired, rr)
		desiredOwners[rr.Name] = struct{}{}
	}

	// Fetch existing rrsets and find owners of this type to delete if not present in desired
	existing, err := c.GetZoneRRSets(ctx, zone)
	if err != nil {
		return err
	}
	deletes := make([]rrset, 0)
	for _, ex := range existing {
		if ex.Type != string(rs.Spec.RecordType) {
			continue
		}
		name := ex.Name // already absolute from PDNS
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

	// Compose patch payload (deterministic order helps debugging/tests)
	patch := append(desired, deletes...)
	sort.Slice(patch, func(i, j int) bool {
		if patch[i].Type != patch[j].Type {
			return patch[i].Type < patch[j].Type
		}
		if patch[i].Name != patch[j].Name {
			return patch[i].Name < patch[j].Name
		}
		// DELETEs last so REPLACEs win when both accidentally appear
		if patch[i].ChangeType != patch[j].ChangeType {
			return patch[i].ChangeType < patch[j].ChangeType
		}
		return false
	})

	payload := patchZoneRequest{RRSets: patch}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		c.BaseURL+"/api/v1/servers/localhost/zones/"+zone+".", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		errBody := readRespBody(resp, 64<<10) // closes Body
		// include status + body so tests/logs show the real PDNS error
		return &pdnsAPIError{Status: resp.StatusCode, Body: errBody}
	}
	_ = resp.Body.Close()
	return nil
}

func buildRRSets(zone string, rs dnsv1alpha1.DNSRecordSet) []rrset {
	type ownerKey = string
	setsByOwner := make(map[ownerKey]*rrset, len(rs.Spec.Records))

	getOrInit := func(owner string, ttl int) *rrset {
		if existing, ok := setsByOwner[owner]; ok {
			return existing
		}
		r := &rrset{
			Name:       owner,
			Type:       string(rs.Spec.RecordType),
			TTL:        ttl,
			ChangeType: "REPLACE",
			Records:    []rrsetRecord{},
		}
		setsByOwner[owner] = r
		return r
	}

	for _, rec := range rs.Spec.Records {
		ttl := 300
		if rec.TTL != nil {
			ttl = int(*rec.TTL)
		}
		name := qualifyOwner(rec.Name, zone)
		r := getOrInit(name, ttl)

		switch rs.Spec.RecordType {
		case dnsv1alpha1.RRTypeA:
			if rec.A == nil {
				continue
			}
			v := strings.TrimSpace(rec.A.Content)
			if v != "" {
				r.Records = append(r.Records, rrsetRecord{Content: v, Disabled: false})
			}

		case dnsv1alpha1.RRTypeAAAA:
			if rec.AAAA == nil {
				continue
			}
			v := strings.TrimSpace(rec.AAAA.Content)
			if v != "" {
				r.Records = append(r.Records, rrsetRecord{Content: v, Disabled: false})
			}

		case dnsv1alpha1.RRTypeCNAME:
			if rec.CNAME == nil {
				continue
			}
			target := strings.TrimSpace(rec.CNAME.Content)
			target = qualifyIfNeeded(target)
			if target != "" {
				// TODO: Technically this is a violation of the RFC, but we'll allow it for now.
				r.Records = append(r.Records, rrsetRecord{Content: target, Disabled: false})
			}

		case dnsv1alpha1.RRTypeTXT:
			if rec.TXT == nil {
				continue
			}
			if s := strings.TrimSpace(rec.TXT.Content); s != "" {
				r.Records = append(r.Records, rrsetRecord{
					Content:  quoteIfNeeded(s),
					Disabled: false,
				})
			}

		case dnsv1alpha1.RRTypeMX:
			if rec.MX == nil {
				continue
			}
			exch := strings.TrimSpace(rec.MX.Exchange)
			if exch != "" {
				line := fmt.Sprintf("%d %s", rec.MX.Preference, qualifyIfNeeded(exch))
				r.Records = append(r.Records, rrsetRecord{Content: line, Disabled: false})
			}

		case dnsv1alpha1.RRTypeSRV:
			if rec.SRV == nil {
				continue
			}
			tgt := strings.TrimSpace(rec.SRV.Target)
			if tgt != "" {
				line := fmt.Sprintf(
					"%d %d %d %s",
					rec.SRV.Priority,
					rec.SRV.Weight,
					rec.SRV.Port,
					qualifyIfNeeded(tgt),
				)
				r.Records = append(r.Records, rrsetRecord{Content: line, Disabled: false})
			}

		case dnsv1alpha1.RRTypeCAA:
			if rec.CAA == nil {
				continue
			}
			line := fmt.Sprintf(
				"%d %s %s",
				rec.CAA.Flag,
				rec.CAA.Tag,
				quoteIfNeeded(rec.CAA.Value),
			)
			r.Records = append(r.Records, rrsetRecord{Content: line, Disabled: false})

		case dnsv1alpha1.RRTypeNS:
			if rec.NS == nil {
				continue
			}
			v := strings.TrimSpace(rec.NS.Content)
			if v != "" {
				r.Records = append(r.Records, rrsetRecord{
					Content:  qualifyIfNeeded(v),
					Disabled: false,
				})
			}

		case dnsv1alpha1.RRTypeSOA:
			if rec.SOA == nil {
				continue
			}

			mname := qualifyIfNeeded(strings.TrimSpace(rec.SOA.MName))
			rname := qualifyIfNeeded(strings.TrimSpace(rec.SOA.RName))

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

			line := fmt.Sprintf(
				"%s %s %s %d %d %d %d",
				mname, rname, serial, refresh, retry, expire, minimum,
			)

			// SOA should be single-valued for a given owner; last one wins.
			r.Records = []rrsetRecord{{Content: line, Disabled: false}}

		case dnsv1alpha1.RRTypePTR:
			if rec.PTR == nil {
				continue
			}
			v := strings.TrimSpace(rec.PTR.Content)
			if v != "" {
				r.Records = append(r.Records, rrsetRecord{
					Content:  qualifyIfNeeded(v),
					Disabled: false,
				})
			}
		case dnsv1alpha1.RRTypeTLSA:
			if rec.TLSA == nil {
				continue
			}
			line := fmt.Sprintf(
				"%d %d %d %s",
				rec.TLSA.Usage,
				rec.TLSA.Selector,
				rec.TLSA.MatchingType,
				rec.TLSA.CertData,
			)
			r.Records = append(r.Records, rrsetRecord{Content: line, Disabled: false})

		case dnsv1alpha1.RRTypeHTTPS:
			if rec.HTTPS == nil {
				continue
			}
			line := encodeSvcbLine(rec.HTTPS.Priority, rec.HTTPS.Target, rec.HTTPS.Params)
			r.Records = append(r.Records, rrsetRecord{Content: line, Disabled: false})

		case dnsv1alpha1.RRTypeSVCB:
			if rec.SVCB == nil {
				continue
			}
			line := encodeSvcbLine(rec.SVCB.Priority, rec.SVCB.Target, rec.SVCB.Params)
			r.Records = append(r.Records, rrsetRecord{Content: line, Disabled: false})
		}
	}

	// Convert map to slice with stable order by owner name.
	out := make([]rrset, 0, len(setsByOwner))
	owners := make([]string, 0, len(setsByOwner))
	for owner := range setsByOwner {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	for _, owner := range owners {
		out = append(out, *setsByOwner[owner])
	}
	return out
}

var (
	svcbFlagKeys    = map[string]struct{}{"no-default-alpn": {}}
	svcbUnquotedCSV = map[string]struct{}{"alpn": {}, "ipv4hint": {}, "ipv6hint": {}, "port": {}}
	svcbQuotedKeys  = map[string]struct{}{"esnikeys": {}, "ech": {}}
)

// rank keys in PDNS-style canonical order
func svcbKeyRank(k string) int {
	switch k {
	case "alpn":
		return 10
	case "no-default-alpn":
		return 20
	case "port":
		return 30
	case "esnikeys", "ech":
		return 40
	case "ipv4hint":
		return 50
	case "ipv6hint":
		return 60
	default:
		return 1000 // unknowns after known ones
	}
}

func encodeSvcbParams(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := svcbKeyRank(keys[i]), svcbKeyRank(keys[j])
		if ri != rj {
			return ri < rj
		}
		// stable within same rank
		return keys[i] < keys[j]
	})

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := strings.TrimSpace(m[k])
		if _, isFlag := svcbFlagKeys[k]; isFlag {
			parts = append(parts, k)
			continue
		}
		if v == "" {
			continue
		}
		if _, unq := svcbUnquotedCSV[k]; unq {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
			continue
		}
		if _, q := svcbQuotedKeys[k]; q {
			parts = append(parts, fmt.Sprintf("%s=%s", k, quoteIfNeeded(v)))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, quoteIfNeeded(v)))
	}
	return strings.Join(parts, " ")
}

func encodeSvcbLine(priority uint16, target string, params map[string]string) string {
	// target: "." for service-form with no alias; otherwise hostname (no trailing dot)
	t := strings.TrimSpace(target)
	switch t {
	case ".":
		// service-form: literal "." must be preserved
		// (do not strip)
	case "":
		// default to service-form with no alias
		t = "."
	default:
		t = qualifyIfNeeded(t)
	}

	// alias form: priority 0 => MUST have a target and MUST NOT have params
	if priority == 0 {
		return fmt.Sprintf("%d %s", priority, t)
	}

	p := encodeSvcbParams(params)
	if p != "" {
		return fmt.Sprintf("%d %s %s", priority, t, p)
	}
	return fmt.Sprintf("%d %s", priority, t)
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

func stripTrailingDot(s string) string {
	if strings.HasSuffix(s, ".") {
		return s[:len(s)-1]
	}
	return s
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
