package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/agentdock/agentdock-verify/internal/lease"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/store"
	"github.com/agentdock/agentdock-verify/internal/worker"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("agentdock-worker", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	databaseURL := flags.String("database-url", os.Getenv("AGENTDOCK_DATABASE_URL"), "PostgreSQL database URL")
	workerID := flags.String("worker-id", "", "unique, non-reusable process-incarnation Worker identifier")
	runID := flags.String("run-id", "", "Run identifier to reconcile")
	leaseTTL := flags.Duration("lease-ttl", 2*time.Second, "Run lease TTL")
	heartbeat := flags.Duration("heartbeat", 500*time.Millisecond, "Worker heartbeat and lease renewal interval")
	poll := flags.Duration("poll", 20*time.Millisecond, "poll interval while paused or waiting for a lease")
	actionDelay := flags.Duration("action-delay", 0, "test-only deterministic action delay")
	postActionDelay := flags.Duration("post-action-delay", 0, "test-only delay after an action returns and before Receipt persistence")
	artifactReceiptDelay := flags.Duration(
		"artifact-receipt-delay",
		0,
		"test-only ApplyPatch delay after Artifact publication and before Receipt persistence",
	)
	staleTokenProbe := flags.Uint64("stale-token-probe", 0, "demo-only: attempt one fenced append with this previously held token")
	artifactRoot := flags.String(
		"artifact-root",
		filepath.Join(os.TempDir(), "agentdock-phase3-artifacts"),
		"directory for phase-3 receipt Artifacts",
	)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *databaseURL == "" || *workerID == "" || *runID == "" {
		fmt.Fprintln(os.Stderr, "--database-url, --worker-id, and --run-id are required")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	eventStore, err := store.NewPostgresEventStore(ctx, *databaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer eventStore.Close()
	fmt.Printf("event_store_ready worker=%s run=%s\n", *workerID, *runID)
	manager, err := lease.NewPostgresManager(ctx, *databaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer manager.Close()
	fmt.Printf("lease_manager_ready worker=%s run=%s\n", *workerID, *runID)
	if *staleTokenProbe > 0 {
		presented := lease.Lease{
			RunID:        *runID,
			WorkerID:     *workerID,
			FencingToken: *staleTokenProbe,
		}
		validationErr := manager.Validate(ctx, presented)
		if validationErr == nil {
			fmt.Fprintln(os.Stderr, "refusing stale-token probe because the presented authority is currently valid")
			return 1
		}
		if !errors.Is(validationErr, lease.ErrStaleLease) &&
			!errors.Is(validationErr, lease.ErrLeaseExpired) {
			fmt.Fprintf(os.Stderr, "stale-token precheck error = %v\n", validationErr)
			return 1
		}
		state, err := eventStore.Rebuild(ctx, *runID)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_, err = eventStore.Append(ctx, state.Run.Version, domain.Event{
			RunID:          *runID,
			Type:           domain.EventLeaseRenewed,
			IdempotencyKey: fmt.Sprintf("stale-token-probe:%s:%d", *workerID, *staleTokenProbe),
			WorkerID:       *workerID,
			FencingToken:   *staleTokenProbe,
		})
		if !errors.Is(err, store.ErrStaleFencingToken) {
			fmt.Fprintf(os.Stderr, "stale token probe error = %v, want %v\n", err, store.ErrStaleFencingToken)
			return 1
		}
		fmt.Printf(
			"stale_probe_rejected worker=%s run=%s token=%d error=%v\n",
			*workerID,
			*runID,
			*staleTokenProbe,
			err,
		)
		return 0
	}

	artifacts, err := store.NewPostgresArtifactStore(eventStore, *artifactRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	options := controller.ManagedOptions{
		Executor:  controller.NewArtifactActionExecutor(reasoner.NewFakeReasoner(), artifacts),
		Artifacts: artifacts,
	}
	if *actionDelay > 0 {
		options.Executor = delayedExecutor{
			before:        *actionDelay,
			after:         *postActionDelay,
			artifactAfter: *artifactReceiptDelay,
			next:          options.Executor,
		}
	} else if *postActionDelay > 0 || *artifactReceiptDelay > 0 {
		options.Executor = delayedExecutor{
			after:         *postActionDelay,
			artifactAfter: *artifactReceiptDelay,
			next:          options.Executor,
		}
	}
	runtime := controller.NewManaged(eventStore, reasoner.NewFakeReasoner(), manager, options)
	err = worker.Run(ctx, manager, runtime, worker.Config{
		WorkerID:          *workerID,
		RunID:             *runID,
		LeaseTTL:          *leaseTTL,
		HeartbeatInterval: *heartbeat,
		PollInterval:      *poll,
		Output:            os.Stdout,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type delayedExecutor struct {
	before        time.Duration
	after         time.Duration
	artifactAfter time.Duration
	next          controller.ActionExecutor
}

func (executor delayedExecutor) Execute(
	ctx context.Context,
	request controller.ActionRequest,
) (lease.ActionReceipt, error) {
	if err := waitDelay(ctx, executor.before); err != nil {
		return lease.ActionReceipt{}, err
	}
	receipt, err := executor.next.Execute(ctx, request)
	if err != nil {
		return lease.ActionReceipt{}, err
	}
	if err := waitDelay(ctx, executor.after); err != nil {
		return lease.ActionReceipt{}, err
	}
	if request.Command.Type == domain.CommandApplyPatch {
		if err := waitDelay(ctx, executor.artifactAfter); err != nil {
			return lease.ActionReceipt{}, err
		}
	}
	return receipt, nil
}

func waitDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
