// SPDX-License-Identifier: AGPL-3.0-only

package pdns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/usage"
)

const usageMetadataListConcurrency = 8

var _ usage.IdentityStore = (*Client)(nil)

type metadataObject struct {
	Kind     string   `json:"kind"`
	Metadata []string `json:"metadata"`
}

type zoneListItem struct {
	Name string `json:"name"`
}

func (c *Client) stampUsageIdentity(ctx context.Context, zone dnsv1alpha1.DNSZone) error {
	id, ok := usage.IdentityFromZone(&zone)
	if !ok {
		return nil
	}
	payload, ok := usage.MarshalIdentity(id)
	if !ok {
		return nil
	}
	return c.setMetadata(ctx, zone.Spec.DomainName, usage.MetadataKind, []string{payload})
}

func (c *Client) ListUsageIdentities(ctx context.Context) ([]usage.ZoneIdentity, error) {
	names, err := c.listZoneNames(ctx)
	if err != nil {
		return nil, err
	}
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(usageMetadataListConcurrency)
	var mu sync.Mutex
	out := make([]usage.ZoneIdentity, 0, len(names))
	for _, name := range names {
		g.Go(func() error {
			values, err := c.getMetadata(ctx, name, usage.MetadataKind)
			if err != nil || len(values) == 0 {
				return nil
			}
			id, ok := usage.UnmarshalIdentity(name, values[0])
			if !ok {
				return nil
			}
			mu.Lock()
			out = append(out, id)
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) listZoneNames(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/servers/localhost/zones?rrsets=false", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pdns list zones failed: status %d", resp.StatusCode)
	}
	var zones []zoneListItem
	if err := json.NewDecoder(resp.Body).Decode(&zones); err != nil {
		return nil, fmt.Errorf("decoding pdns zone list: %w", err)
	}
	names := make([]string, 0, len(zones))
	for _, z := range zones {
		if z.Name != "" {
			names = append(names, z.Name)
		}
	}
	return names, nil
}

func (c *Client) setMetadata(ctx context.Context, zone, kind string, values []string) error {
	id := absoluteZoneID(zone)
	if id == "" || kind == "" {
		return nil
	}
	body, err := json.Marshal(metadataObject{Kind: kind, Metadata: values})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.metadataURL(id, kind), bytes.NewReader(body))
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
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("pdns set metadata %s failed: status %d: %s", kind, resp.StatusCode, readRespBody(resp, 16<<10))
	}
	return nil
}

func (c *Client) getMetadata(ctx context.Context, zone, kind string) ([]string, error) {
	id := absoluteZoneID(zone)
	if id == "" || kind == "" {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.metadataURL(id, kind), nil)
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
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pdns get metadata %s failed: status %d: %s", kind, resp.StatusCode, readRespBody(resp, 16<<10))
	}
	var obj metadataObject
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return nil, fmt.Errorf("decoding pdns metadata: %w", err)
	}
	return obj.Metadata, nil
}

func (c *Client) metadataURL(zoneID, kind string) string {
	return c.BaseURL + "/api/v1/servers/localhost/zones/" + url.PathEscape(zoneID) + "/metadata/" + url.PathEscape(kind)
}

func absoluteZoneID(zone string) string {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return ""
	}
	return strings.TrimSuffix(zone, ".") + "."
}
