package downstreamclient

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch

var _ ResourceStrategy = &mappedNamespaceResourceStrategy{}

type mappedNamespaceResourceStrategy struct {
	upstreamClusterName string
	upstreamClient      client.Client
	downstreamClient    client.Client
}

func NewMappedNamespaceResourceStrategy(
	upstreamClusterName string,
	upstreamClient client.Client,
	downstreamClient client.Client,
) ResourceStrategy {
	return &mappedNamespaceResourceStrategy{
		upstreamClusterName: upstreamClusterName,
		upstreamClient:      upstreamClient,
		downstreamClient:    downstreamClient,
	}
}

func (c *mappedNamespaceResourceStrategy) GetClient() client.Client {
	return &mappedNamespaceClient{
		client:           c.downstreamClient,
		resourceStrategy: c,
	}
}

func (c *mappedNamespaceResourceStrategy) ObjectMetaFromUpstreamObject(ctx context.Context, obj metav1.Object) (metav1.ObjectMeta, error) {
	downstreamNamespaceName, err := c.getDownstreamNamespaceName(ctx, obj)
	if err != nil {
		return metav1.ObjectMeta{}, fmt.Errorf("failed to get downstream namespace name: %w", err)
	}

	return metav1.ObjectMeta{
		Name:      obj.GetName(),
		Namespace: downstreamNamespaceName,
		Labels: map[string]string{
			UpstreamOwnerNamespaceLabel: obj.GetNamespace(),
		},
	}, nil
}

func (c *mappedNamespaceResourceStrategy) getUpstreamNamespace(ctx context.Context, obj metav1.Object) (*corev1.Namespace, error) {
	namespace := &corev1.Namespace{}

	if obj == nil {
		return nil, fmt.Errorf("object is nil")
	}
	if c.upstreamClient == nil {
		return nil, fmt.Errorf("upstream client is nil")
	}
	if err := c.upstreamClient.Get(ctx, client.ObjectKey{Name: obj.GetNamespace()}, namespace); err != nil {
		return nil, fmt.Errorf("failed to get upstream namespace: %w", err)
	}

	return namespace, nil
}

func (c *mappedNamespaceResourceStrategy) getDownstreamNamespaceName(ctx context.Context, obj metav1.Object) (string, error) {
	namespace, err := c.getUpstreamNamespace(ctx, obj)
	if err != nil {
		return "", fmt.Errorf("failed to get downstream namespace: %w", err)
	}

	return fmt.Sprintf("ns-%s", namespace.UID), nil
}

func (c *mappedNamespaceResourceStrategy) ensureDownstreamNamespace(ctx context.Context, obj metav1.Object) (*corev1.Namespace, error) {
	downstreamNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: obj.GetNamespace(),
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, c.downstreamClient, downstreamNamespace, func() error {
		if downstreamNamespace.Labels == nil {
			downstreamNamespace.Labels = make(map[string]string)
		}

		downstreamNamespace.Labels[UpstreamOwnerClusterNameLabel] = fmt.Sprintf("cluster-%s", strings.ReplaceAll(c.upstreamClusterName, "/", "_"))

		labels := obj.GetLabels()
		if v, ok := labels[UpstreamOwnerNamespaceLabel]; ok {
			downstreamNamespace.Labels[UpstreamOwnerNamespaceLabel] = v
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to ensure downstream namespace: %w", err)
	}

	return downstreamNamespace, nil
}

const (
	UpstreamOwnerClusterNameLabel = "meta.datumapis.com/upstream-cluster-name"
	UpstreamOwnerGroupLabel       = "meta.datumapis.com/upstream-group"
	UpstreamOwnerKindLabel        = "meta.datumapis.com/upstream-kind"
	UpstreamOwnerNameLabel        = "meta.datumapis.com/upstream-name"
	UpstreamOwnerNamespaceLabel   = "meta.datumapis.com/upstream-namespace"
)

func (c *mappedNamespaceResourceStrategy) SetControllerReference(ctx context.Context, owner, controlled metav1.Object, opts ...controllerutil.OwnerReferenceOption) error {
	// TODO(jreese) add owner validation

	if owner.GetNamespace() == "" || controlled.GetNamespace() == "" {
		return fmt.Errorf("cluster scoped resource controllers are not supported")
	}

	// For simplicity, we use a ConfigMap for an anchor. This may change to a
	// separate type in the future if ConfigMap bloat causes an issue in caches.

	gvk, err := apiutil.GVKForObject(owner.(runtime.Object), c.upstreamClient.Scheme())
	if err != nil {
		return err
	}

	anchorName := fmt.Sprintf("anchor-%s", owner.GetUID())

	anchorLabels := map[string]string{
		UpstreamOwnerClusterNameLabel: fmt.Sprintf("cluster-%s", strings.ReplaceAll(c.upstreamClusterName, "/", "_")),
		UpstreamOwnerGroupLabel:       gvk.Group,
		UpstreamOwnerKindLabel:        gvk.Kind,
		UpstreamOwnerNameLabel:        owner.GetName(),
		UpstreamOwnerNamespaceLabel:   owner.GetNamespace(),
	}

	downstreamClient := c.GetClient()

	var anchorConfigMap corev1.ConfigMap
	if err := downstreamClient.Get(ctx, client.ObjectKey{Namespace: controlled.GetNamespace(), Name: anchorName}, &anchorConfigMap); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed listing configmaps: %w", err)
	}

	if anchorConfigMap.CreationTimestamp.IsZero() {
		anchorConfigMap.Name = anchorName
		anchorConfigMap.Labels = anchorLabels
		anchorConfigMap.Namespace = controlled.GetNamespace()
		if err := downstreamClient.Create(ctx, &anchorConfigMap); err != nil {
			return fmt.Errorf("failed creating anchor configmap: %w", err)
		}
	}

	if err := controllerutil.SetOwnerReference(&anchorConfigMap, controlled, downstreamClient.Scheme(), opts...); err != nil {
		return fmt.Errorf("failed setting anchor owner reference: %w", err)
	}

	labels := controlled.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}

	labels[UpstreamOwnerClusterNameLabel] = anchorLabels[UpstreamOwnerClusterNameLabel]
	labels[UpstreamOwnerGroupLabel] = anchorLabels[UpstreamOwnerGroupLabel]
	labels[UpstreamOwnerKindLabel] = anchorLabels[UpstreamOwnerKindLabel]
	labels[UpstreamOwnerNameLabel] = anchorLabels[UpstreamOwnerNameLabel]
	labels[UpstreamOwnerNamespaceLabel] = anchorLabels[UpstreamOwnerNamespaceLabel]
	controlled.SetLabels(labels)

	return nil
}

func (c *mappedNamespaceResourceStrategy) SetOwnerReference(ctx context.Context, owner, object metav1.Object, opts ...controllerutil.OwnerReferenceOption) error {
	return controllerutil.SetOwnerReference(owner, object, c.downstreamClient.Scheme(), opts...)
}

// DeleteAnchorForObject will delete the anchor configmap associated with the
// provided owner, which will help drive GC of other entities.
func (c *mappedNamespaceResourceStrategy) DeleteAnchorForObject(
	ctx context.Context,
	owner client.Object,
) error {

	anchorName := fmt.Sprintf("anchor-%s", owner.GetUID())

	downstreamObjectMeta, err := c.ObjectMetaFromUpstreamObject(ctx, owner)
	if err != nil {
		return fmt.Errorf("failed to get downstream object metadata: %w", err)
	}

	downstreamClient := c.GetClient()

	var configMap corev1.ConfigMap
	if err := downstreamClient.Get(ctx, client.ObjectKey{Namespace: downstreamObjectMeta.Namespace, Name: anchorName}, &configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed listing configmaps: %w", err)
	}

	return downstreamClient.Delete(ctx, &configMap)
}

var _ client.Client = &mappedNamespaceClient{}

type mappedNamespaceClient struct {
	client           client.Client
	resourceStrategy *mappedNamespaceResourceStrategy
}

func (c *mappedNamespaceClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	return c.client.Apply(ctx, obj, opts...)
}

func (c *mappedNamespaceClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	_, err := c.resourceStrategy.ensureDownstreamNamespace(ctx, obj)
	if err != nil {
		return fmt.Errorf("failed to ensure downstream namespace: %w", err)
	}

	return c.client.Create(ctx, obj, opts...)
}

func (c *mappedNamespaceClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return c.client.Delete(ctx, obj, opts...)
}

func (c *mappedNamespaceClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	return c.client.DeleteAllOf(ctx, obj, opts...)
}

func (c *mappedNamespaceClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.client.Get(ctx, key, obj, opts...)
}

func (c *mappedNamespaceClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.client.List(ctx, list, opts...)
}

func (c *mappedNamespaceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	return c.client.Patch(ctx, obj, patch, opts...)
}

func (c *mappedNamespaceClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.client.Update(ctx, obj, opts...)
}

func (c *mappedNamespaceClient) GroupVersionKindFor(obj runtime.Object) (schema.GroupVersionKind, error) {
	return c.client.GroupVersionKindFor(obj)
}

func (c *mappedNamespaceClient) IsObjectNamespaced(obj runtime.Object) (bool, error) {
	return c.client.IsObjectNamespaced(obj)
}

func (c *mappedNamespaceClient) Scheme() *runtime.Scheme {
	return c.client.Scheme()
}

func (c *mappedNamespaceClient) RESTMapper() meta.RESTMapper {
	return c.client.RESTMapper()
}

func (c *mappedNamespaceClient) Status() client.SubResourceWriter {
	return c.client.Status()
}

func (c *mappedNamespaceClient) SubResource(subResource string) client.SubResourceClient {
	return c.client.SubResource(subResource)
}
