package serving

import (
	"testing"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestRetiredContributionFenceRejectsEveryReplay(t *testing.T) {
	a := &Agent{state: newCheckpoint()}
	retirement := model.PublicationPlan{ObservationFences: []model.ObservationFence{{ContributionUID: "endpoint", Generation: 2, Sequence: 2, Retired: true}}}
	a.commitContributionFences(retirement)
	for _, generation := range []uint64{2, 3} {
		plan := model.PublicationPlan{ObservationFences: []model.ObservationFence{{ContributionUID: "endpoint", Generation: generation, Sequence: generation}}}
		if err := a.validateContributionFences(plan); err == nil {
			t.Fatal("retired contribution returned through a newer snapshot")
		}
	}
	if !a.state.ContributionFences["endpoint"].Retired {
		t.Fatal("checkpoint fence lost retirement")
	}
}
