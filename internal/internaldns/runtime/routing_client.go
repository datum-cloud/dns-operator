package runtime

import (
	"context"

	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RoutingClient keeps project resources in their source API server while
// storing serving artifacts in the durable platform control plane. It never
// mirrors or rewrites API-assigned UIDs and generations.
type RoutingClient struct {
	client.Client
	Source            client.Client
	PlatformNamespace string
}

func (c *RoutingClient) target(obj k8sruntime.Object) client.Client {
	switch obj.(type) {
	case *dnsv1.DNSZone, *dnsv1.DNSZoneList, *dnsv1.DNSZoneAssociation, *dnsv1.DNSZoneAssociationList,
		*dnsv1.DNSRecordSet, *dnsv1.DNSRecordSetList, *dnsv1.DNSResolverContext, *dnsv1.DNSResolverContextList,
		*dnsv1.DNSResolverAccessBinding, *dnsv1.DNSResolverAccessBindingList,
		*dnsv1.DNSRegistration, *dnsv1.DNSRegistrationList, *dnsv1.DNSNamingPolicy, *dnsv1.DNSNamingPolicyList,
		*dnsv1.DNSRecordContribution, *dnsv1.DNSRecordContributionList:
		return c.Source
	default:
		return c.Client
	}
}
func (c *RoutingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.target(obj).Get(ctx, key, obj, opts...)
}
func (c *RoutingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.target(list).List(ctx, list, opts...)
}
func (c *RoutingClient) Apply(ctx context.Context, obj k8sruntime.ApplyConfiguration, opts ...client.ApplyOption) error {
	return c.Client.Apply(ctx, obj, opts...)
}
func (c *RoutingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return c.target(obj).Create(ctx, obj, opts...)
}
func (c *RoutingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return c.target(obj).Delete(ctx, obj, opts...)
}
func (c *RoutingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.target(obj).Update(ctx, obj, opts...)
}
func (c *RoutingClient) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
	return c.target(obj).Patch(ctx, obj, p, opts...)
}
func (c *RoutingClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	return c.target(obj).DeleteAllOf(ctx, obj, opts...)
}
func (c *RoutingClient) Status() client.SubResourceWriter {
	return &routingSubresource{parent: c, name: "status"}
}
func (c *RoutingClient) SubResource(name string) client.SubResourceClient {
	return &routingSubresource{parent: c, name: name}
}

type routingSubresource struct {
	parent *RoutingClient
	name   string
}

func (s *routingSubresource) Get(ctx context.Context, obj client.Object, sub client.Object, opts ...client.SubResourceGetOption) error {
	return s.parent.target(obj).SubResource(s.name).Get(ctx, obj, sub, opts...)
}
func (s *routingSubresource) Create(ctx context.Context, obj client.Object, sub client.Object, opts ...client.SubResourceCreateOption) error {
	return s.parent.target(obj).SubResource(s.name).Create(ctx, obj, sub, opts...)
}
func (s *routingSubresource) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return s.parent.target(obj).SubResource(s.name).Update(ctx, obj, opts...)
}
func (s *routingSubresource) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
	return s.parent.target(obj).SubResource(s.name).Patch(ctx, obj, p, opts...)
}
func (s *routingSubresource) Apply(ctx context.Context, obj k8sruntime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	return s.parent.Client.SubResource(s.name).Apply(ctx, obj, opts...)
}

var _ client.Client = (*RoutingClient)(nil)
