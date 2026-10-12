package serving

import (
	"encoding/json"
	"fmt"
	"sort"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

// bindingFence retains source authority independently of shard snapshot ordering.
// Omission withdraws access without inventing a source-issued sequence number.
type bindingFence struct {
	Identity                string              `json:"identity"`
	ConfigurationGeneration uint64              `json:"configurationGeneration"`
	ConfigurationRevision   uint64              `json:"configurationRevision"`
	Authorization           model.Authorization `json:"authorization"`
	Tombstone               bool                `json:"tombstone"`
}

func bindingIdentity(b model.Binding) string {
	transports := append([]model.Transport(nil), b.Transports...)
	sort.Slice(transports, func(i, j int) bool { return transports[i] < transports[j] })
	data, _ := json.Marshal(struct {
		Project, Context, Node, Regional string
		Port                             uint16
		Transports                       []model.Transport
	}{b.ProjectUID, b.ContextUID, b.ConsumerAddress, b.ClusterAddress, b.Port, transports})
	return model.Hash(data)
}

func bindingFenceFor(b model.Binding) bindingFence {
	return bindingFence{Identity: bindingIdentity(b), ConfigurationGeneration: b.BindingGeneration, ConfigurationRevision: b.ConfigurationRevision, Authorization: b.Authorization, Tombstone: b.Tombstone}
}

func advanceBindingFences(prior map[string]bindingFence, snapshot model.ServingSnapshot) (map[string]bindingFence, error) {
	next := make(map[string]bindingFence, len(prior)+len(snapshot.Bindings))
	for uid, old := range prior {
		withdrawn := old
		withdrawn.Tombstone = true
		next[uid] = withdrawn
	}
	for _, b := range snapshot.Bindings {
		candidate := bindingFenceFor(b)
		if old, ok := prior[b.BindingUID]; ok {
			if candidate.Identity != old.Identity {
				return nil, fmt.Errorf("binding %s changed its UID-pinned source or listeners", b.BindingUID)
			}
			if candidate.ConfigurationGeneration < old.ConfigurationGeneration || candidate.ConfigurationRevision < old.ConfigurationRevision {
				return nil, fmt.Errorf("binding %s configuration regressed", b.BindingUID)
			}
			cmp := model.Compare(candidate.Authorization.IssuerEpoch, candidate.Authorization.Revision, old.Authorization.IssuerEpoch, old.Authorization.Revision)
			if cmp < 0 {
				return nil, fmt.Errorf("binding %s source authorization regressed", b.BindingUID)
			}
			if cmp == 0 && !candidate.Authorization.ValidUntil.Equal(old.Authorization.ValidUntil) {
				return nil, fmt.Errorf("binding %s changed the original deadline at the same source fence", b.BindingUID)
			}
			if old.Tombstone && !candidate.Tombstone && cmp <= 0 {
				return nil, fmt.Errorf("binding %s reactivation requires a newer source authorization", b.BindingUID)
			}
			if old.Tombstone != candidate.Tombstone && candidate.ConfigurationRevision <= old.ConfigurationRevision {
				return nil, fmt.Errorf("binding %s withdrawal or reactivation requires a newer configuration revision", b.BindingUID)
			}
		}
		next[b.BindingUID] = candidate
	}
	return next, nil
}
