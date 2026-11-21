// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.miloapis.com/dns/internal/internaldns/platform"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	regionalAddressReleaseInterval = 10 * time.Minute
	// regionalAddressGrace outlasts the gap between a claim and its binding.
	regionalAddressGrace = 10 * time.Minute
)

var regionalAddressClaims = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "internal_dns_regional_address_claims",
	Help: "Regional resolver address claims seen by the latest release sweep. state is in_use, or releasable for what a dry run would free.",
}, []string{"state"})

var regionalAddressReleases = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_regional_address_releases_total",
	Help: "Regional resolver address claims and leases freed, and missing claims restored for live bindings. Dry runs are not counted.",
}, []string{"action"})

var regionalAddressConflicts = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "internal_dns_regional_address_conflicts",
	Help: "Routed regional addresses whose claim names a different owner, seen by the latest release sweep.",
})

func init() {
	metrics.Registry.MustRegister(regionalAddressClaims, regionalAddressReleases, regionalAddressConflicts)
}

func regionalAddressRelease(allocator *platform.Allocator, dryRun bool) func(context.Context) error {
	return func(ctx context.Context) error {
		result, err := allocator.ReleaseRegionalAddresses(ctx, time.Now(), regionalAddressGrace, dryRun)
		// A sweep that could not list anything has nothing to report.
		if err == nil || result != (platform.RegionalRelease{}) {
			regionalAddressClaims.WithLabelValues("in_use").Set(float64(result.InUse))
			regionalAddressConflicts.Set(float64(result.Conflicts))
			if dryRun {
				regionalAddressClaims.WithLabelValues("releasable").Set(float64(result.Released))
			} else {
				regionalAddressClaims.WithLabelValues("releasable").Set(0)
				regionalAddressReleases.WithLabelValues("released").Add(float64(result.Released))
				regionalAddressReleases.WithLabelValues("restored").Add(float64(result.Restored))
			}
		}
		if result.Released > 0 || result.Restored > 0 || result.Conflicts > 0 {
			ctrl.LoggerFrom(ctx).WithName("internal-dns").Info("regional address release sweep", "dryRun", dryRun, "inUse", result.InUse, "released", result.Released, "restored", result.Restored, "conflicts", result.Conflicts)
		}
		return err
	}
}
