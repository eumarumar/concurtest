package failure_test

import (
	"context"
	"errors"
	"testing"

	"github.com/eumarumar/concurtest/internal/failure"
)

func TestErrorRetainsCodesMessagesAndCauses(t *testing.T) {
	t.Parallel()

	leaf := errors.New("network unavailable")
	err := failure.Join(
		failure.CodeOperationBatchFailed,
		"execute operations",
		failure.Wrap(failure.CodeRequestSendFailed, "send request", leaf),
		context.Canceled,
	)
	if got := err.Error(); got != "execute operations: send request: network unavailable\ncontext canceled" {
		t.Fatalf("Error() = %q", got)
	}
	if !errors.Is(err, leaf) || !errors.Is(err, context.Canceled) {
		t.Fatal("structured error does not retain its causes")
	}
	if got := failure.CodeOf(err); got != failure.CodeOperationBatchFailed {
		t.Fatalf("CodeOf() = %q", got)
	}
	if got := failure.CodeOf(context.DeadlineExceeded); got != failure.CodeDeadlineExceeded {
		t.Fatalf("CodeOf(deadline) = %q", got)
	}
}

func TestJoinIgnoresNilCauses(t *testing.T) {
	t.Parallel()

	if err := failure.Join(failure.CodeInternal, "unused", nil, nil); err != nil {
		t.Fatalf("Join() = %v, want nil", err)
	}
}
