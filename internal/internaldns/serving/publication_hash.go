// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"sort"
	"strings"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func effectivePlanHash(plan model.PublicationPlan) string {
	var values []string
	for _, rr := range plan.RRSets {
		for _, r := range rr.Records {
			if !r.Eligible {
				continue
			}
			values = append(values, model.AbsoluteName(rr.Name)+"\x00"+strings.ToUpper(rr.Type)+"\x00"+r.Content)
		}
	}
	sort.Strings(values)
	return model.Hash([]byte(strings.Join(values, "\n")))
}
