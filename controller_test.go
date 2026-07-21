// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

const workerTestTimeout = 5 * time.Second

func TestRunListenerAndScalerStopsListenerBeforeScaler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	listenerStarted := make(chan struct{})
	scalerStarted := make(chan struct{})
	listenerStopped := make(chan struct{})
	scalerObservedOrder := make(chan bool, 1)

	type result struct {
		listenerErr error
		scalerErr   error
	}
	resultCh := make(chan result, 1)
	go func() {
		listenerErr, scalerErr := runListenerAndScaler(
			ctx,
			func(ctx context.Context) error {
				close(listenerStarted)
				<-ctx.Done()
				close(listenerStopped)
				return ctx.Err()
			},
			func(ctx context.Context) error {
				close(scalerStarted)
				<-ctx.Done()
				select {
				case <-listenerStopped:
					scalerObservedOrder <- true
				default:
					scalerObservedOrder <- false
				}
				return ctx.Err()
			},
		)
		resultCh <- result{listenerErr, scalerErr}
	}()

	waitForSignal(t, listenerStarted, "listener start")
	waitForSignal(t, scalerStarted, "scaler start")
	cancel()

	var got result
	select {
	case got = <-resultCh:
	case <-time.After(workerTestTimeout):
		t.Fatal("timed out waiting for workers to stop")
	}
	if !errors.Is(got.listenerErr, context.Canceled) {
		t.Fatalf("listener returned %v, want context cancellation", got.listenerErr)
	}
	if !errors.Is(got.scalerErr, context.Canceled) {
		t.Fatalf("scaler returned %v, want context cancellation", got.scalerErr)
	}
	if !<-scalerObservedOrder {
		t.Fatal("scaler stopped before listener")
	}
}

func TestRunListenerAndScalerStopsListenerWhenScalerExits(t *testing.T) {
	listenerStarted := make(chan struct{})
	scalerErr := errors.New("scaler stopped")

	listenerResult, scalerResult := runListenerAndScaler(
		context.Background(),
		func(ctx context.Context) error {
			close(listenerStarted)
			<-ctx.Done()
			return ctx.Err()
		},
		func(context.Context) error {
			<-listenerStarted
			return scalerErr
		},
	)

	if !errors.Is(listenerResult, context.Canceled) {
		t.Fatalf(
			"listener returned %v, want context cancellation",
			listenerResult,
		)
	}
	if !errors.Is(scalerResult, scalerErr) {
		t.Fatalf("scaler returned %v, want %v", scalerResult, scalerErr)
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(workerTestTimeout):
		t.Fatalf("timed out waiting for %s", name)
	}
}
