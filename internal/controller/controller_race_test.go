package controller_test

import (
	"context"
	"sync"
	"testing"

	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/store"
)

func TestConcurrentReadsDuringAppendAreRaceFree(t *testing.T) {
	ctx := context.Background()
	reconciler := controller.New(store.NewMemoryEventStore(), reasoner.NewFakeReasoner())
	mustCreate(t, ctx, reconciler, "run-race-controller")

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_, _ = reconciler.GetRun(ctx, "run-race-controller")
			}
		}()
	}
	for range 6 {
		if _, err := reconciler.Reconcile(ctx, "run-race-controller"); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	wg.Wait()
}
