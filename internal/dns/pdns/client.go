package pdns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	dnserrors "go.miloapis.com/dns-operator/internal/dns/errors"
	dnsutils "go.miloapis.com/dns-operator/internal/dns/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	logger  logr.Logger
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		logger:  logf.FromContext(context.TODO(), "client", "powerdns"),
	}
}

type pdnsAPIError struct {
	Status int
	Body   string
}

var ACCOUNT_OBSERVED_GENERATION = "OBSERVED_GENERATION"
var ACCOUNT_OWNER = "OWNER"

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

// Init initializes the PDNS client.
func (c *Client) Init() error {
	return nil
}

// Shutdown is a no-op for the PDNS client.
func (c *Client) Shutdown() {}

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
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusConflict {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
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

	if resp.StatusCode == http.StatusNotFound {
		return "", dnserrors.ErrZoneNotFound
	}

	if resp.StatusCode != 200 {
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

func (c *Client) EnsureZone(ctx context.Context, zone dnsv1alpha1.DNSZone, class dnsv1alpha1.DNSZoneClass) error {
	// Get desired nameservers from the class spec
	nss := c.GetZoneNameservers(ctx, zone, class)

	if _, err := c.GetZone(ctx, zone.Spec.DomainName); err != nil {
		if errors.Is(err, dnserrors.ErrZoneNotFound) {
			// Zone does not exist, create it
			if err := c.CreateZone(ctx, zone.Spec.DomainName, nss); err != nil {
				return err
			}
			return nil
		}
		return err
	}

	// TODO -> Implement zone update logic if needed (e.g., updating nameservers)

	return nil
}

func (c *Client) DeleteZone(ctx context.Context, zone dnsv1alpha1.DNSZone) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.BaseURL+"/api/v1/servers/localhost/zones/"+zone.Spec.DomainName+".", nil)
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
	if resp.StatusCode == http.StatusNoContent {
		return nil // deleted successfully
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pdns delete zone failed: status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) GetZoneNameservers(ctx context.Context, zone dnsv1alpha1.DNSZone, class dnsv1alpha1.DNSZoneClass) []string {
	var desiredNS []string
	if class.Spec.NameServerPolicy != nil &&
		class.Spec.NameServerPolicy.Mode == dnsv1alpha1.NameServerPolicyModeStatic &&
		class.Spec.NameServerPolicy.Static != nil {
		desiredNS = append(desiredNS, class.Spec.NameServerPolicy.Static.Servers...)
	}

	return dnsutils.NormalizeStringSlice(desiredNS)
}

func (c *Client) EnsureRecordSet(ctx context.Context, zone dnsv1alpha1.DNSZone, recordSet dnsv1alpha1.DNSRecordSet) ([]dnsv1alpha1.RecordSetStatus, error) {
	statusList := make([]dnsv1alpha1.RecordSetStatus, 0, len(recordSet.Spec.Records))

	rrsetMap := make(map[string][]dnsv1alpha1.RecordEntry)

	for i := range recordSet.Spec.Records {
		owner := recordSet.Spec.Records[i].Name
		rrsetMap[owner] = append(rrsetMap[owner], recordSet.Spec.Records[i])
	}

	for owner, entries := range rrsetMap {
		ownerRRSet, ok := BuildOwnerRRSet(zone.Spec.DomainName, recordSet.Spec.RecordType, owner, entries)

		if !ok {
			c.logger.Info("Failed to build owner RRSet", "owner", owner, "rrset", ownerRRSet)
			statusList = append(statusList, recordSetErrorStatus(owner, fmt.Errorf("failed to build owner RRSet for owner %s", owner), metav1.ConditionFalse))
			continue
		}

		c.logger.Info("Ensuring record set for owner", "owner", owner, "rrset", ownerRRSet)
		zones, err := c.getPDNSRRSet(ctx, zone.Spec.DomainName, owner, recordSet.Spec.RecordType)
		c.logger.Info("Fetched existing rrsets from PDNS", "zone", zone.Spec.DomainName, "owner", owner, "rrsets", zones, "err", err)

		if err != nil {
			// Set programmed to False on API errors
			statusList = append(statusList, recordSetErrorStatus(owner, err, metav1.ConditionFalse))
			continue
		}

		if len(zones) == 0 {
			// Not configured. Easy path we configure it and return
			statusList = append(statusList, c.replaceRRSetStatus(ctx, zone.Spec.DomainName, recordSet, owner, ownerRRSet))
		} else {
			// Zones returned.
			if len(zones) > 1 {
				// This should not happen. We should only have one RRSet per owner per type. Log an error and continue.
				c.logger.Error(fmt.Errorf("multiple rrsets returned for owner %s", owner), "zone", zone.Spec.DomainName, "owner", owner, "rrsets", zones)
				statusList = append(statusList, recordSetErrorStatus(owner, fmt.Errorf("multiple RRSets returned for owner %s", owner), metav1.ConditionFalse))
				continue
			}

			if zones[0].Comments == nil {
				// this happens when the record set was created previously and doesn't have the OWNER comment. We will replace it with the new one.
				statusList = append(statusList, c.replaceRRSetStatus(ctx, zone.Spec.DomainName, recordSet, owner, ownerRRSet))
			} else {
				needsReplace := true
				for _, comment := range zones[0].Comments {
					// Check if the comment contains the observed generation
					if comment.Account == ACCOUNT_OBSERVED_GENERATION {
						observedGeneration, err := strconv.ParseInt(comment.Content, 10, 64)
						if err != nil {
							c.logger.Error(err, "Failed to parse observed generation", "comment", comment.Content)
							break
						}
						if observedGeneration == recordSet.Generation {
							// Observed generation matches, no need to replace
							needsReplace = false
						}
						break
					}
				}

				if needsReplace {
					c.logger.Info("RRSet Needs Replacement. Observed generation does not match current generation", "owner", owner, "observedGeneration", zones[0].Comments, "currentGeneration", recordSet.Generation)
					statusList = append(statusList, c.replaceRRSetStatus(ctx, zone.Spec.DomainName, recordSet, owner, ownerRRSet))
				} else {
					statusList = append(statusList, recordSetSuccessStatus(owner, recordSet.Status.RecordSets))
				}
			}
		}
	}

	// Deletion Phase
	curRecordSet, err := c.queryDNSByComment(ctx, fmt.Sprintf("%s:%s", recordSet.Namespace, recordSet.Name))

	if err != nil {
		c.logger.Error(err, "Failed to query PDNS for record set", "recordSet", recordSet.Name)
		return nil, err
	}

	for _, cur := range curRecordSet {
		if cur.Type != string(recordSet.Spec.RecordType) {
			// Ignore this record type
			continue
		}

		if !slices.ContainsFunc(statusList, func(s dnsv1alpha1.RecordSetStatus) bool {
			QualifiedOwner := QualifyOwner(s.Name, zone.Spec.DomainName)
			return QualifiedOwner == cur.Name
		}) {
			// This owner is not in the desired state, delete it
			c.logger.Info("Deleting RecordSet from PowerDNS", "owner", cur.Name, "recordType", cur.Type, "recordSet", recordSet.Name)
			err := c.DeleteRRSet(ctx, zone.Spec.DomainName, string(recordSet.Spec.RecordType), cur.Name)
			if err != nil {
				c.logger.Error(err, "failed to delete recordset from PowerDNS")
			}
		}
	}

	return statusList, nil
}

func makeProgrammedStatus(owner string, status metav1.ConditionStatus, reason, message string) dnsv1alpha1.RecordSetStatus {
	return dnsv1alpha1.RecordSetStatus{
		Name: owner,
		Conditions: []metav1.Condition{{
			Type:               "Programmed",
			Status:             status,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		}},
	}
}

func recordSetErrorStatus(owner string, err error, status metav1.ConditionStatus) dnsv1alpha1.RecordSetStatus {
	return makeProgrammedStatus(owner, status, "PDNSError", err.Error())
}

func recordSetCreatedStatus(owner string) dnsv1alpha1.RecordSetStatus {
	return makeProgrammedStatus(owner, metav1.ConditionTrue, "Programmed", "Record set successfully created")
}

func recordSetSuccessStatus(owner string, current []dnsv1alpha1.RecordSetStatus) dnsv1alpha1.RecordSetStatus {
	for _, curStatus := range current {
		if curStatus.Name != owner {
			continue
		}
		if len(curStatus.Conditions) == 0 || curStatus.Conditions[0].Status != metav1.ConditionTrue {
			return recordSetCreatedStatus(owner)
		}
		return curStatus
	}
	return recordSetCreatedStatus(owner)
}

func (c *Client) replaceRRSetStatus(
	ctx context.Context,
	zoneName string,
	recordSet dnsv1alpha1.DNSRecordSet,
	owner string,
	ownerRRSet OwnerRRSet,
) dnsv1alpha1.RecordSetStatus {
	err := c.ReplaceRRSet(
		ctx,
		zoneName,
		string(recordSet.Spec.RecordType),
		owner,
		ownerRRSet.TTL,
		ownerRRSet.Records,
		fmt.Sprintf("%s:%s", recordSet.Namespace, recordSet.Name),
		recordSet.Generation,
	)
	if err != nil {
		return recordSetErrorStatus(owner, err, metav1.ConditionFalse)
	}
	return recordSetCreatedStatus(owner)
}

func (c *Client) getPDNSRRSet(ctx context.Context, zoneName string, rrset_name string, rrset_type dnsv1alpha1.RRType) ([]zoneRRset, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/servers/localhost/zones/"+zoneName+".", nil)
	if err != nil {
		return nil, err
	}
	query := req.URL.Query()
	query.Set("rrset_name", rrset_name)
	query.Set("rrset_type", string(rrset_type))
	req.URL.RawQuery = query.Encode()
	req.Header.Set("X-API-Key", c.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, dnserrors.ErrZoneNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &pdnsAPIError{Status: resp.StatusCode, Body: readRespBody(resp, 64<<10)}
	}

	var zr zoneResponse
	if err := json.NewDecoder(resp.Body).Decode(&zr); err != nil {
		return nil, err
	}
	return zr.RRSets, nil
}

func (c *Client) queryDNSByComment(ctx context.Context, commentContent string) ([]queryResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/servers/localhost/search-data", nil)
	if err != nil {
		return nil, err
	}
	query := req.URL.Query()
	query.Set("q", commentContent)
	query.Set("max", fmt.Sprintf("%d", 9999))
	query.Set("object_type", "comment")
	req.URL.RawQuery = query.Encode()
	req.Header.Set("X-API-Key", c.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, dnserrors.ErrZoneNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &pdnsAPIError{Status: resp.StatusCode, Body: readRespBody(resp, 64<<10)}
	}

	var qr []queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		return nil, err
	}
	return qr, nil
}

func (c *Client) DeleteRecordSet(ctx context.Context, zone dnsv1alpha1.DNSZone, recordSet dnsv1alpha1.DNSRecordSet) error {
	rrsetMap := make(map[string][]dnsv1alpha1.RecordEntry)

	for i := range recordSet.Spec.Records {
		owner := recordSet.Spec.Records[i].Name
		rrsetMap[owner] = append(rrsetMap[owner], recordSet.Spec.Records[i])
	}

	for owner := range rrsetMap {
		patch := []rrset{{
			Name:       QualifyOwner(owner, zone.Spec.DomainName),
			Type:       string(recordSet.Spec.RecordType),
			ChangeType: "DELETE",
			Records:    []rrsetRecord{},
		}}
		err := c.applyRRSetPatch(ctx, zone.Spec.DomainName, patch)
		if err != nil {
			c.logger.Error(err, "Failed to delete record set from PowerDNS", "owner", owner, "recordType", recordSet.Spec.RecordType)
			return err
		}
	}

	curRecordSet, err := c.queryDNSByComment(ctx, fmt.Sprintf("%s:%s", recordSet.Namespace, recordSet.Name))
	if err != nil {
		c.logger.Error(err, "Failed to query record set by comment from PowerDNS", "recordSet", recordSet.Name)
		return err
	}

	// In case there are any extra owners in PDNS that are not in the desired state, we need to delete them as well.
	for _, cur := range curRecordSet {
		if cur.Type != string(recordSet.Spec.RecordType) {
			// Ignore this record type
			continue
		}

		patch := []rrset{{
			Name:       cur.Name,
			Type:       cur.Type,
			ChangeType: "DELETE",
			Records:    []rrsetRecord{},
		}}
		err := c.applyRRSetPatch(ctx, zone.Spec.DomainName, patch)
		if err != nil {
			c.logger.Error(err, "Failed to delete record set from PowerDNS", "owner", cur.Name, "recordType", cur.Type)
			return err
		}
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

// BuildOwnerRRSet converts the provided record entries for a single owner name into PDNS payload data.
func BuildOwnerRRSet(
	zone string,
	recordType dnsv1alpha1.RRType,
	ownerName string,
	entries []dnsv1alpha1.RecordEntry,
) (OwnerRRSet, bool) {
	if len(entries) == 0 {
		return OwnerRRSet{}, false
	}
	rs := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: recordType,
			Records:    entries,
		},
	}
	rrsets := buildRRSets(zone, rs)
	target := QualifyOwner(ownerName, zone)
	for _, rr := range rrsets {
		if rr.Name != target {
			continue
		}
		values := make([]string, 0, len(rr.Records))
		for _, rec := range rr.Records {
			values = append(values, rec.Content)
		}
		return OwnerRRSet{
			TTL:     rr.TTL,
			Records: values,
		}, true
	}
	return OwnerRRSet{}, false
}

type rrsetRecord struct {
	Content  string `json:"content"`
	Disabled bool   `json:"disabled"`
}

type rrset struct {
	Name       string             `json:"name"`
	Type       string             `json:"type"`
	TTL        int                `json:"ttl"`
	ChangeType string             `json:"changetype"`
	Records    []rrsetRecord      `json:"records"`
	Comments   []zoneRRsetComment `json:"comments,omitempty"`
}

type patchZoneRequest struct {
	RRSets []rrset `json:"rrsets"`
}

// OwnerRRSet captures the PDNS-ready view of a single owner name.
type OwnerRRSet struct {
	TTL     int
	Records []string
}

// Structures for GET zone response parsing
type zoneResponse struct {
	Name   string      `json:"name"`
	RRSets []zoneRRset `json:"rrsets"`
}

type zoneRRset struct {
	Name     string             `json:"name"`
	Type     string             `json:"type"`
	TTL      int                `json:"ttl"`
	Records  []zoneRRsetRecord  `json:"records"`
	Comments []zoneRRsetComment `json:"comments"`
}

type zoneRRsetComment struct {
	Account    string `json:"account"`
	Content    string `json:"content"`
	ModifiedAt int    `json:"modified_at"`
}

type zoneRRsetRecord struct {
	Content  string `json:"content"`
	Disabled bool   `json:"disabled"`
}

type queryResponse struct {
	Content    string `json:"content"`
	Name       string `json:"name"`
	ObjectType string `json:"object_type"`
	Type       string `json:"type"`
	Zone       string `json:"zone"`
	ZoneId     string `json:"zone_id"`
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

	return c.applyRRSetPatch(ctx, zone, patch)
}

// ReplaceRRSet ensures a single (type, owner) RRset matches the provided values exactly.
func (c *Client) ReplaceRRSet(
	ctx context.Context,
	zone string,
	recordType string,
	ownerName string,
	ttl int,
	values []string,
	ownerRef string,
	observedGeneration int64,
) error {
	records := make([]rrsetRecord, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		records = append(records, rrsetRecord{Content: v, Disabled: false})
	}
	patch := []rrset{{
		Name:       QualifyOwner(ownerName, zone),
		Type:       recordType,
		TTL:        ttl,
		ChangeType: "REPLACE",
		Records:    dedupeRecords(records),
		Comments: []zoneRRsetComment{{
			Account:    ACCOUNT_OWNER,
			Content:    ownerRef,
			ModifiedAt: int(time.Now().Unix()),
		}, {
			Account:    ACCOUNT_OBSERVED_GENERATION,
			Content:    fmt.Sprintf("%d", observedGeneration),
			ModifiedAt: int(time.Now().Unix()),
		}},
	}}
	return c.applyRRSetPatch(ctx, zone, patch)
}

// DeleteRRSet removes the referenced (type, owner) RRset from PDNS.
func (c *Client) DeleteRRSet(ctx context.Context, zone, recordType, ownerName string) error {
	patch := []rrset{{
		Name:       QualifyOwner(ownerName, zone),
		Type:       recordType,
		ChangeType: "DELETE",
		Records:    []rrsetRecord{},
	}}
	return c.applyRRSetPatch(ctx, zone, patch)
}

func (c *Client) applyRRSetPatch(ctx context.Context, zone string, patch []rrset) error {
	if len(patch) == 0 {
		return nil
	}
	sort.Slice(patch, func(i, j int) bool {
		if patch[i].Type != patch[j].Type {
			return patch[i].Type < patch[j].Type
		}
		if patch[i].Name != patch[j].Name {
			return patch[i].Name < patch[j].Name
		}
		return patch[i].ChangeType < patch[j].ChangeType
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

	c.logger.Info("Applied RRSet patch to PowerDNS", "zone", zone, "patch", string(body), "status", resp.StatusCode)

	if resp.StatusCode == http.StatusNoContent {
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		errBody := readRespBody(resp, 64<<10) // closes Body
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
		name := QualifyOwner(rec.Name, zone)
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
			if target != "" && len(r.Records) == 0 {
				// CNAME is single-valued (RFC 1034): keep exactly one record.
				// First non-empty entry wins; extras/duplicates are dropped, as
				// PowerDNS rejects a multi-valued or duplicate CNAME RRset (422).
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

		case dnsv1alpha1.RRTypeALIAS:
			if rec.ALIAS == nil {
				continue
			}
			target := strings.TrimSpace(rec.ALIAS.Content)
			target = qualifyIfNeeded(target)
			if target != "" && len(r.Records) == 0 {
				// ALIAS is single-valued at an owner name: keep one record.
				// First non-empty entry wins; extras/duplicates are dropped.
				r.Records = append(r.Records, rrsetRecord{Content: target, Disabled: false})
			}

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
		r := setsByOwner[owner]
		r.Records = dedupeRecords(r.Records)
		out = append(out, *r)
	}
	return out
}

// dedupeRecords removes records with duplicate Content within a single RRset,
// preserving first-seen order. PowerDNS rejects the whole RRset with HTTP 422
// ("duplicate record with content ...") if the same content appears twice, so
// the payload must be de-duplicated before it is sent.
func dedupeRecords(in []rrsetRecord) []rrsetRecord {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, rec := range in {
		if _, ok := seen[rec.Content]; ok {
			continue
		}
		seen[rec.Content] = struct{}{}
		out = append(out, rec)
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

// QualifyOwner returns the absolute RRset name PowerDNS keys an owner on within
// zone. It accepts every spelling the API allows: "@" or the empty string for
// the apex, a relative label such as "api", or an already-absolute name ending
// in a dot. Several spellings therefore collapse to one RRset — "api" and
// "api.example.com." both qualify to "api.example.com." in zone example.com —
// so callers comparing two owner names for RRset identity must compare their
// qualified forms rather than the raw values.
func QualifyOwner(owner, zone string) string {
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
	return fmt.Sprintf("\"%s\"", escapeTXTContent(s))
}

// escapeTXTContent escapes semicolons that are special in PowerDNS zone-file
// presentation format. If the user already escaped a semicolon as \; we leave
// it alone to be idempotent.
func escapeTXTContent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == ';' && (i == 0 || s[i-1] != '\\') {
			b.WriteString(`\;`)
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
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
