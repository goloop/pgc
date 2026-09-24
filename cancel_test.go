package main

import (
	"context"
	"testing"
	"time"
)

// A run that is over must not be cancelled, even when its context ends right
// after - which is what the deferred calls of every run do.
func TestCancelOnDoneIgnoresAFinishedRun(t *testing.T) {
	for range 1000 {
		ctx, stop := context.WithCancel(context.Background())
		finished := make(chan struct{})
		called := make(chan struct{}, 1)
		done := make(chan struct{})
		go func() {
			cancelOnDone(ctx, finished, func() { called <- struct{}{} })
			close(done)
		}()
		close(finished)
		stop()
		<-done
		select {
		case <-called:
			t.Fatal("a finished run was cancelled")
		default:
		}
	}
}

// A context that ends while the run is going cancels it.
func TestCancelOnDoneCancelsARunningStatement(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	finished := make(chan struct{})
	called := make(chan struct{})
	go cancelOnDone(ctx, finished, func() { close(called) })
	stop()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("cancel was not called")
	}
	close(finished)
}
