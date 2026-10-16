// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"time"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// deadlineClient bounds every direct source API operation without imposing a
// short lifetime on the manager's long-running watch connections.
type deadlineClient struct {
	client.Client
	Timeout time.Duration
}

func (c *deadlineClient) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= c.Timeout {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.Timeout)
}

func (c *deadlineClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	ctx, cancel := c.bounded(ctx)
	defer cancel()
	return c.Client.Get(ctx, key, obj, opts...)
}
func (c *deadlineClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	ctx, cancel := c.bounded(ctx)
	defer cancel()
	return c.Client.List(ctx, list, opts...)
}
func (c *deadlineClient) Apply(ctx context.Context, obj k8sruntime.ApplyConfiguration, opts ...client.ApplyOption) error {
	ctx, cancel := c.bounded(ctx)
	defer cancel()
	return c.Client.Apply(ctx, obj, opts...)
}
func (c *deadlineClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	ctx, cancel := c.bounded(ctx)
	defer cancel()
	return c.Client.Create(ctx, obj, opts...)
}
func (c *deadlineClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	ctx, cancel := c.bounded(ctx)
	defer cancel()
	return c.Client.Delete(ctx, obj, opts...)
}
func (c *deadlineClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	ctx, cancel := c.bounded(ctx)
	defer cancel()
	return c.Client.Update(ctx, obj, opts...)
}
func (c *deadlineClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	ctx, cancel := c.bounded(ctx)
	defer cancel()
	return c.Client.Patch(ctx, obj, patch, opts...)
}
func (c *deadlineClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	ctx, cancel := c.bounded(ctx)
	defer cancel()
	return c.Client.DeleteAllOf(ctx, obj, opts...)
}
func (c *deadlineClient) Status() client.SubResourceWriter {
	return &deadlineSubresourceWriter{client: c, subresource: c.Client.Status()}
}
func (c *deadlineClient) SubResource(name string) client.SubResourceClient {
	return &deadlineSubresource{client: c, subresource: c.Client.SubResource(name)}
}

type deadlineSubresource struct {
	client      *deadlineClient
	subresource client.SubResourceClient
}

type deadlineSubresourceWriter struct {
	client      *deadlineClient
	subresource client.SubResourceWriter
}

func (s *deadlineSubresourceWriter) Create(ctx context.Context, obj client.Object, sub client.Object, opts ...client.SubResourceCreateOption) error {
	ctx, cancel := s.client.bounded(ctx)
	defer cancel()
	return s.subresource.Create(ctx, obj, sub, opts...)
}
func (s *deadlineSubresourceWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	ctx, cancel := s.client.bounded(ctx)
	defer cancel()
	return s.subresource.Update(ctx, obj, opts...)
}
func (s *deadlineSubresourceWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	ctx, cancel := s.client.bounded(ctx)
	defer cancel()
	return s.subresource.Patch(ctx, obj, patch, opts...)
}
func (s *deadlineSubresourceWriter) Apply(ctx context.Context, obj k8sruntime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	ctx, cancel := s.client.bounded(ctx)
	defer cancel()
	return s.subresource.Apply(ctx, obj, opts...)
}

func (s *deadlineSubresource) Get(ctx context.Context, obj client.Object, sub client.Object, opts ...client.SubResourceGetOption) error {
	ctx, cancel := s.client.bounded(ctx)
	defer cancel()
	return s.subresource.Get(ctx, obj, sub, opts...)
}
func (s *deadlineSubresource) Create(ctx context.Context, obj client.Object, sub client.Object, opts ...client.SubResourceCreateOption) error {
	ctx, cancel := s.client.bounded(ctx)
	defer cancel()
	return s.subresource.Create(ctx, obj, sub, opts...)
}
func (s *deadlineSubresource) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	ctx, cancel := s.client.bounded(ctx)
	defer cancel()
	return s.subresource.Update(ctx, obj, opts...)
}
func (s *deadlineSubresource) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	ctx, cancel := s.client.bounded(ctx)
	defer cancel()
	return s.subresource.Patch(ctx, obj, patch, opts...)
}
func (s *deadlineSubresource) Apply(ctx context.Context, obj k8sruntime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	ctx, cancel := s.client.bounded(ctx)
	defer cancel()
	return s.subresource.Apply(ctx, obj, opts...)
}

var _ client.Client = (*deadlineClient)(nil)
