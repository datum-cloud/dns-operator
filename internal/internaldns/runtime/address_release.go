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
	Help: "Regional resolver address claims seen by the latest release sweep. state is in_use, or releasable for claims nothing routes (freed unless the sweep is a dry run).",
}, []string{"state"})

var regionalAddressReleases = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_regional_address_releases_total",
	Help: "Regional resolver address claims freed, or counted on a dry run, and missing claims restored for live bindings.",
}, []string{"action", "dry_run"})

func init() {
	metrics.Registry.MustRegister(regionalAddressClaims, regionalAddressReleases)
}

func regionalAddressRelease(allocator *platform.Allocator, dryRun bool) func(context.Context) error {
	return func(ctx context.Context) error {
		result, err := allocator.ReleaseRegionalAddresses(ctx, time.Now(), regionalAddressGrace, dryRun)
		mode := "false"
		if dryRun {
			mode = "true"
		}
		regionalAddressClaims.WithLabelValues("in_use").Set(float64(result.InUse))
		regionalAddressClaims.WithLabelValues("releasable").Set(float64(result.Released))
		regionalAddressReleases.WithLabelValues("released", mode).Add(float64(result.Released))
		regionalAddressReleases.WithLabelValues("restored", mode).Add(float64(result.Restored))
		if result.Released > 0 || result.Restored > 0 {
			ctrl.LoggerFrom(ctx).WithName("internal-dns").Info("regional address release sweep", "dryRun", dryRun, "inUse", result.InUse, "released", result.Released, "restored", result.Restored)
		}
		return err
	}
}
