package runtime

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var operationResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_operations_total",
	Help: "Completed internal DNS operations by configured loop and result.",
}, []string{"operation", "result"})

var operationLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "internal_dns_operation_last_success_timestamp_seconds",
	Help: "Unix timestamp of the latest successful internal DNS operation.",
}, []string{"operation"})

func init() {
	metrics.Registry.MustRegister(operationResults, operationLastSuccess)
}

func observeOperation(operation string, err error) {
	lastSuccess := operationLastSuccess.WithLabelValues(operation)
	result := "success"
	if err != nil {
		result = "error"
	} else {
		lastSuccess.Set(float64(time.Now().Unix()))
	}
	operationResults.WithLabelValues(operation, result).Inc()
}
