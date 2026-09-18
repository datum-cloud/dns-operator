// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Reader fetches the DNS objects the tools reason over.
//
// The identity a read runs under is decided by whoever constructs the
// Reader, never buried in tool code: the server builds one per request from
// the caller's own credentials, so a tool call can never see more than the
// caller could see themselves.
type Reader interface {
	// ListZones returns every DNSZone in the namespace.
	ListZones(ctx context.Context, namespace string) ([]dnsv1alpha1.DNSZone, error)
	// GetZone returns one DNSZone by name.
	GetZone(ctx context.Context, namespace, name string) (*dnsv1alpha1.DNSZone, error)
	// ListRecordSets returns every DNSRecordSet in the namespace that belongs
	// to the named zone.
	ListRecordSets(ctx context.Context, namespace, zoneName string) ([]dnsv1alpha1.DNSRecordSet, error)
	// GetRecordSet returns one DNSRecordSet by name.
	GetRecordSet(ctx context.Context, namespace, name string) (*dnsv1alpha1.DNSRecordSet, error)
}

// ClientReader implements Reader against a controller-runtime client.
type ClientReader struct {
	Client client.Client
}

var _ Reader = (*ClientReader)(nil)

// NewClientReader returns a Reader backed by c. Every read is performed with
// whatever credentials c carries.
func NewClientReader(c client.Client) *ClientReader {
	return &ClientReader{Client: c}
}

func (r *ClientReader) ListZones(ctx context.Context, namespace string) ([]dnsv1alpha1.DNSZone, error) {
	var list dnsv1alpha1.DNSZoneList
	if err := r.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing zones in %s: %w", namespace, err)
	}
	return list.Items, nil
}

func (r *ClientReader) GetZone(ctx context.Context, namespace, name string) (*dnsv1alpha1.DNSZone, error) {
	var z dnsv1alpha1.DNSZone
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := r.Client.Get(ctx, key, &z); err != nil {
		return nil, fmt.Errorf("getting zone %s/%s: %w", namespace, name, err)
	}
	return &z, nil
}

// ListRecordSets filters on spec.dnsZoneRef.name rather than a label. The
// record set carries the reference in its spec, and filtering client-side
// avoids depending on a field index the MCP server does not register.
func (r *ClientReader) ListRecordSets(
	ctx context.Context, namespace, zoneName string,
) ([]dnsv1alpha1.DNSRecordSet, error) {
	var list dnsv1alpha1.DNSRecordSetList
	if err := r.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing record sets in %s: %w", namespace, err)
	}
	out := make([]dnsv1alpha1.DNSRecordSet, 0, len(list.Items))
	for _, rs := range list.Items {
		if rs.Spec.DNSZoneRef.Name == zoneName {
			out = append(out, rs)
		}
	}
	return out, nil
}

func (r *ClientReader) GetRecordSet(ctx context.Context, namespace, name string) (*dnsv1alpha1.DNSRecordSet, error) {
	var rs dnsv1alpha1.DNSRecordSet
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := r.Client.Get(ctx, key, &rs); err != nil {
		return nil, fmt.Errorf("getting record set %s/%s: %w", namespace, name, err)
	}
	return &rs, nil
}
