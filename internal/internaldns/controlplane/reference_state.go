// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const referenceStateLabel = "internal-dns.miloapis.com/reference-state"
const referenceProjectLabel = "internal-dns.miloapis.com/project-uid"
const referenceStateDataKey = "references.json"
const defaultPlatformNamespace = "internal-dns-system"

type referenceState struct {
	Unverified      bool                                `json:"unverified,omitempty"`
	SourceNamespace string                              `json:"sourceNamespace"`
	SourceKind      string                              `json:"sourceKind"`
	Source          dnsv1.DNSObjectReference            `json:"source"`
	References      map[string]dnsv1.DNSObjectReference `json:"references"`
}

func referenceKind(source client.Object) string {
	switch source.(type) {
	case *dnsv1.DNSZoneAssociation:
		return "DNSZoneAssociation"
	case *dnsv1.DNSNamingPolicy:
		return "DNSNamingPolicy"
	case *dnsv1.DNSRegistration:
		return "DNSRegistration"
	default:
		return ""
	}
}

func (r *Reconciler) referenceStateObject(source client.Object) *corev1.ConfigMap {
	namespace := r.Options.PlatformNamespace
	if namespace == "" {
		namespace = defaultPlatformNamespace
	}
	identity := fmt.Sprintf("%s/%s/%s/%s/%s", r.Options.ProjectUID, source.GetNamespace(), referenceKind(source), source.GetName(), source.GetUID())
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: "references-" + model.OpaqueToken(identity),
		Labels: map[string]string{referenceStateLabel: "true", referenceProjectLabel: string(r.Options.ProjectUID)},
	}}
}

func (r *Reconciler) readReferences(ctx context.Context, source client.Object) (map[string]dnsv1.DNSObjectReference, error) {
	_, state, err := r.loadReferenceState(ctx, source)
	return state.References, err
}

func (r *Reconciler) loadReferenceState(ctx context.Context, source client.Object) (*corev1.ConfigMap, referenceState, error) {
	object := r.referenceStateObject(source)
	state := referenceState{SourceKind: referenceKind(source), SourceNamespace: source.GetNamespace(), Source: dnsv1.DNSObjectReference{Name: source.GetName(), UID: source.GetUID()}, References: map[string]dnsv1.DNSObjectReference{}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		if apierrors.IsNotFound(err) {
			state.Unverified = hasResolvedReferenceStatus(source)
		}
		return object, state, client.IgnoreNotFound(err)
	}
	if err := json.Unmarshal([]byte(object.Data[referenceStateDataKey]), &state); err != nil {
		return object, state, fmt.Errorf("decode DNS reference state: %w", err)
	}
	if state.SourceKind != referenceKind(source) || state.SourceNamespace != source.GetNamespace() || state.Source.Name != source.GetName() || state.Source.UID != source.GetUID() {
		return object, state, fmt.Errorf("DNS reference state source does not match")
	}
	return object, state, nil
}

func (r *Reconciler) pinReferences(ctx context.Context, source client.Object, selected map[string]dnsv1.DNSObjectReference) (map[string]dnsv1.DNSObjectReference, bool, error) {
	object, state, err := r.loadReferenceState(ctx, source)
	if err != nil {
		return nil, false, err
	}
	if state.Unverified {
		if object.ResourceVersion == "" {
			payload, err := json.Marshal(state)
			if err != nil {
				return nil, false, err
			}
			object.Data = map[string]string{referenceStateDataKey: string(payload)}
			if err := r.Create(ctx, object); err != nil {
				return nil, false, err
			}
		}
		return state.References, false, nil
	}
	current := true
	resolved := map[string]dnsv1.DNSObjectReference{}
	for key, ref := range selected {
		pin, valid := pinNamedReference(ref.Name, state.References[key], ref)
		resolved[key] = pin
		current = current && valid
	}
	if reflect.DeepEqual(state.References, resolved) {
		return resolved, current, nil
	}
	state.References = resolved
	payload, err := json.Marshal(state)
	if err != nil {
		return nil, false, err
	}
	if object.ResourceVersion == "" {
		object.Data = map[string]string{referenceStateDataKey: string(payload)}
		err = r.Create(ctx, object)
	} else {
		base := object.DeepCopy()
		object.Data[referenceStateDataKey] = string(payload)
		err = r.Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}
	return resolved, current, err
}

func (r *Reconciler) collectReferenceStates(ctx context.Context, sources []client.Object) error {
	live := map[string]bool{}
	for _, source := range sources {
		live[r.referenceStateObject(source).Name] = true
	}
	var states corev1.ConfigMapList
	namespace := r.Options.PlatformNamespace
	if namespace == "" {
		namespace = defaultPlatformNamespace
	}
	if err := r.List(ctx, &states, client.InNamespace(namespace), client.MatchingLabels{referenceStateLabel: "true", referenceProjectLabel: string(r.Options.ProjectUID)}); err != nil {
		return err
	}
	for i := range states.Items {
		if !live[states.Items[i].Name] {
			var state referenceState
			if err := json.Unmarshal([]byte(states.Items[i].Data[referenceStateDataKey]), &state); err != nil {
				ctrl.LoggerFrom(ctx).Error(err, "skip malformed DNS reference state", "name", states.Items[i].Name)
				continue
			}
			var source client.Object
			switch state.SourceKind {
			case "DNSZoneAssociation":
				source = &dnsv1.DNSZoneAssociation{}
			case "DNSNamingPolicy":
				source = &dnsv1.DNSNamingPolicy{}
			case "DNSRegistration":
				source = &dnsv1.DNSRegistration{}
			default:
				ctrl.LoggerFrom(ctx).Error(fmt.Errorf("unknown source kind %q", state.SourceKind), "skip DNS reference state", "name", states.Items[i].Name)
				continue
			}
			source.SetNamespace(state.SourceNamespace)
			source.SetName(state.Source.Name)
			source.SetUID(state.Source.UID)
			if r.referenceStateObject(source).Name != states.Items[i].Name {
				ctrl.LoggerFrom(ctx).Error(fmt.Errorf("source identity mismatch"), "skip DNS reference state", "name", states.Items[i].Name)
				continue
			}
			err := r.Get(ctx, client.ObjectKey{Namespace: state.SourceNamespace, Name: state.Source.Name}, source)
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if err == nil && source.GetUID() == state.Source.UID {
				continue
			}
			if err := r.Delete(ctx, &states.Items[i], client.Preconditions{UID: &states.Items[i].UID}); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}

type associationReferences struct {
	Zone    dnsv1.DNSObjectReference
	Context dnsv1.DNSObjectReference
}

func (r *Reconciler) associationReferences(ctx context.Context, associations []dnsv1.DNSZoneAssociation) (map[types.UID]associationReferences, error) {
	pinned, _ := ctx.Value(pinnedAssociationReferencesKey{}).(map[types.UID]associationReferences)
	result := map[types.UID]associationReferences{}
	for i := range associations {
		if refs, ok := pinned[associations[i].UID]; ok {
			result[associations[i].UID] = refs
			continue
		}
		refs, err := r.readReferences(ctx, &associations[i])
		if err != nil {
			return nil, err
		}
		result[associations[i].UID] = associationReferences{Zone: refs["zone"], Context: refs["context"]}
		if pinned != nil {
			pinned[associations[i].UID] = result[associations[i].UID]
		}
	}
	return result, nil
}

type pinnedAssociationReferencesKey struct{}

// withPinnedAssociationReferences remembers each association's references for
// the zone loop of a pass. Every association is pinned before that loop, so
// the stored references cannot change within it.
func withPinnedAssociationReferences(ctx context.Context) context.Context {
	return context.WithValue(ctx, pinnedAssociationReferencesKey{}, map[types.UID]associationReferences{})
}

func hasResolvedReferenceStatus(source client.Object) bool {
	switch object := source.(type) {
	case *dnsv1.DNSZoneAssociation:
		return object.Status.ResolvedDNSZoneRef.Name != "" || object.Status.ResolvedResolverContextRef.Name != "" || len(object.Status.Conditions) != 0
	case *dnsv1.DNSNamingPolicy:
		return object.Status.ResolvedResolverContextRef.Name != "" || len(object.Status.Conditions) != 0
	case *dnsv1.DNSRegistration:
		return object.Status.CanonicalFQDN != "" || object.Status.ObservedGeneration != 0 || len(object.Status.Conditions) != 0
	default:
		return false
	}
}
