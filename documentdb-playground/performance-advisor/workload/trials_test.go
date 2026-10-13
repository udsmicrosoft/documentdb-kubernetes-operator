// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workload

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMeasuredWindowHonorsPublishedUTCDeadline(t *testing.T) {
	deadline := time.Now().UTC().Add(150 * time.Millisecond)
	calls := 0
	operations, err := measure(context.Background(), deadline, func(context.Context) error {
		calls++
		return nil
	})
	if err != nil || operations != calls || calls < 1 || time.Now().UTC().Before(deadline) {
		t.Fatalf("window ended before its published deadline: calls=%d operations=%d error=%v", calls, operations, err)
	}
}

func TestMeasuredWindowStopsOnCancellationAndFailure(t *testing.T) {
	sentinel := errors.New("synthetic failure")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(sentinel)
	calls, err := measure(ctx, time.Now().Add(time.Second), func(context.Context) error {
		t.Fatal("cancelled workload must not issue a query")
		return nil
	})
	if calls != 0 || !errors.Is(err, sentinel) {
		t.Fatalf("cancellation was not preserved: %d %v", calls, err)
	}
	calls, err = measure(context.Background(), time.Now().Add(time.Second), func(context.Context) error {
		return sentinel
	})
	if calls != 0 || !errors.Is(err, sentinel) {
		t.Fatalf("failed query earned an operation: %d %v", calls, err)
	}
}
