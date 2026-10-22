package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRetryLoopReportsFailureAndRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	operation := t.Name()
	defer operationResults.DeleteLabelValues(operation, "error")
	defer operationResults.DeleteLabelValues(operation, "success")
	defer operationLastSuccess.DeleteLabelValues(operation)
	done := make(chan error, 1)
	calls := 0
	go retryLoop(ctx, time.Millisecond, operation, func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("source unavailable")
		}
		cancel()
		return nil
	}, done)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop")
	}
	if got := testutil.ToFloat64(operationResults.WithLabelValues(operation, "error")); got != 1 {
		t.Fatalf("error count = %v", got)
	}
	if got := testutil.ToFloat64(operationResults.WithLabelValues(operation, "success")); got != 1 {
		t.Fatalf("success count = %v", got)
	}
	if got := testutil.ToFloat64(operationLastSuccess.WithLabelValues(operation)); got == 0 {
		t.Fatal("successful recovery did not update freshness")
	}
}

func TestFailedOperationDoesNotRenewSuccess(t *testing.T) {
	operation := t.Name()
	defer operationResults.DeleteLabelValues(operation, "error")
	defer operationLastSuccess.DeleteLabelValues(operation)
	operationLastSuccess.WithLabelValues(operation).Set(123)
	observeOperation(operation, errors.New("failed"))
	if got := testutil.ToFloat64(operationLastSuccess.WithLabelValues(operation)); got != 123 {
		t.Fatalf("failure renewed successful operation timestamp: %v", got)
	}
}
