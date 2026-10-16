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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const referenceStateLabel = "internal-dns.miloapis.com/reference-state"
const referenceProjectLabel = "internal-dns.miloapis.com/project-uid"
const referenceStateDataKey = "references.json"
const defaultPlatformNamespace = "internal-dns-system"

type referenceState struct {
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
				return err
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
				return fmt.Errorf("unknown DNS reference source kind %q", state.SourceKind)
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
	result := map[types.UID]associationReferences{}
	for i := range associations {
		refs, err := r.readReferences(ctx, &associations[i])
		if err != nil {
			return nil, err
		}
		result[associations[i].UID] = associationReferences{Zone: refs["zone"], Context: refs["context"]}
	}
	return result, nil
}
