// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"

	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func mutateContribution(ctx context.Context, source client.Client, contribution *dnsv1.DNSRecordContribution, mutate func(*dnsv1.DNSRecordContributionSpec)) error {
	key := client.ObjectKeyFromObject(contribution)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := source.Get(ctx, key, contribution); err != nil {
			return err
		}
		before := contribution.DeepCopy()
		mutate(&contribution.Spec)
		return source.Patch(ctx, contribution, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}
