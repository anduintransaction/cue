// Copyright (C) 2014-2026 Anduin Transactions Inc.

package modregistry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func waitWithTimeout[T any](t *testing.T, w *parallelWorker[T], timeout time.Duration) ([]T, error) {
	t.Helper()
	type result struct {
		resp []T
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := w.wait()
		resCh <- result{resp: resp, err: err}
	}()
	select {
	case r := <-resCh:
		return r.resp, r.err
	case <-time.After(timeout):
		t.Fatalf("parallelWorker.wait() deadlocked (waited %s)", timeout)
		return nil, nil
	}
}

// When a task returns an error, parallelWorker.wait() must not deadlock.
//
// Previously, on error the main goroutine would re-send on the 1-capacity
// done channel that was already filled by the wg watcher goroutine once
// all workers finished. The second send had no receiver and hung forever,
// producing "fatal error: all goroutines are asleep - deadlock!".
func TestParallelWorkerErrorNoDeadlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := newParallelWorker[int](ctx)
	taskErr := errors.New("boom")
	w.run(0, func(ctx context.Context) (int, error) {
		return 0, taskErr
	})

	_, err := waitWithTimeout(t, w, 2*time.Second)
	if !errors.Is(err, taskErr) {
		t.Fatalf("expected %v, got %v", taskErr, err)
	}
}

// Multiple successful tasks arriving in any order must not deadlock.
func TestParallelWorkerMultipleSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := newParallelWorker[int](ctx)
	const n = 16
	for i := 0; i < n; i++ {
		i := i
		w.run(i, func(ctx context.Context) (int, error) {
			return i * i, nil
		})
	}

	resp, err := waitWithTimeout(t, w, 2*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp) != n {
		t.Fatalf("expected %d responses, got %d", n, len(resp))
	}
	for i, v := range resp {
		if v != i*i {
			t.Fatalf("unexpected response at %d: got %d, want %d", i, v, i*i)
		}
	}
}

// Error mixed with slow success tasks must still return the error and not
// deadlock waiting on the in-flight success goroutines.
func TestParallelWorkerErrorWithSlowSiblings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := newParallelWorker[int](ctx)
	taskErr := errors.New("boom")

	w.run(0, func(ctx context.Context) (int, error) {
		return 0, taskErr
	})
	for i := 1; i < 4; i++ {
		w.run(i, func(ctx context.Context) (int, error) {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(200 * time.Millisecond):
				return 42, nil
			}
		})
	}

	_, err := waitWithTimeout(t, w, 3*time.Second)
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
}
