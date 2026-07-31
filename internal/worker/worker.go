package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/agentdock/agentdock-verify/internal/lease"
)

type Config struct {
	WorkerID          string
	RunID             string
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
	Output            io.Writer
}

// Run registers one Worker and reconciles one Run until it reaches a terminal
// or operator-owned WaitingApproval state. A replacement process calls the
// same function and the same Controller.ReconcileLeased path.
func Run(
	ctx context.Context,
	manager lease.Manager,
	runtime *controller.Controller,
	config Config,
) error {
	if config.WorkerID == "" ||
		config.RunID == "" ||
		config.LeaseTTL <= 0 ||
		config.HeartbeatInterval <= 0 ||
		config.HeartbeatInterval >= config.LeaseTTL {
		return fmt.Errorf("worker_id, run_id, positive TTL, and heartbeat < TTL are required")
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 10 * time.Millisecond
	}
	if config.Output == nil {
		config.Output = io.Discard
	}
	if _, err := manager.Register(ctx, config.WorkerID); err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	fmt.Fprintf(config.Output, "worker_registered worker=%s\n", config.WorkerID)
	workerCtx, cancelWorker := context.WithCancel(ctx)
	defer cancelWorker()
	heartbeatErrors := make(chan error, 1)
	go workerHeartbeatLoop(
		workerCtx,
		manager,
		config.WorkerID,
		config.HeartbeatInterval,
		heartbeatErrors,
		cancelWorker,
	)

	for {
		select {
		case err := <-heartbeatErrors:
			return err
		default:
		}
		acquired, err := manager.Acquire(workerCtx, config.RunID, config.WorkerID, config.LeaseTTL)
		if errors.Is(err, lease.ErrLeaseHeld) {
			if err := waitContext(workerCtx, config.PollInterval); err != nil {
				select {
				case heartbeatErr := <-heartbeatErrors:
					return heartbeatErr
				default:
					return err
				}
			}
			continue
		}
		if workerCtx.Err() != nil {
			select {
			case heartbeatErr := <-heartbeatErrors:
				return heartbeatErr
			default:
				return err
			}
		}
		if err != nil {
			return fmt.Errorf("acquire Run lease: %w", err)
		}
		fmt.Fprintf(
			config.Output,
			"lease_acquired worker=%s run=%s token=%d takeover=%t\n",
			config.WorkerID,
			config.RunID,
			acquired.FencingToken,
			acquired.TookOver,
		)

		sessionErr := runLeaseSession(workerCtx, manager, runtime, config, acquired.Lease)
		if sessionErr == nil {
			return nil
		}
		if workerCtx.Err() != nil {
			select {
			case heartbeatErr := <-heartbeatErrors:
				return heartbeatErr
			default:
				return workerCtx.Err()
			}
		}
		if errors.Is(sessionErr, lease.ErrLeaseExpired) ||
			errors.Is(sessionErr, lease.ErrStaleLease) {
			fmt.Fprintf(
				config.Output,
				"lease_lost worker=%s run=%s token=%d error=%v\n",
				config.WorkerID,
				config.RunID,
				acquired.FencingToken,
				sessionErr,
			)
			continue
		}
		return sessionErr
	}
}

func runLeaseSession(
	ctx context.Context,
	manager lease.Manager,
	runtime *controller.Controller,
	config Config,
	initial lease.Lease,
) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	session := &leaseState{current: initial}
	renewalErrors := make(chan error, 1)
	go leaseRenewalLoop(
		sessionCtx,
		manager,
		session,
		config.LeaseTTL,
		config.HeartbeatInterval,
		renewalErrors,
		cancel,
	)

	for {
		select {
		case err := <-renewalErrors:
			return err
		default:
		}
		current := session.load()
		result, err := runtime.ReconcileLeased(sessionCtx, config.RunID, current)
		if err != nil {
			if sessionCtx.Err() != nil {
				select {
				case renewalErr := <-renewalErrors:
					return renewalErr
				default:
					return sessionCtx.Err()
				}
			}
			return fmt.Errorf("leased reconcile: %w", err)
		}
		if result.State.Run.ObservedState.Terminal() ||
			result.State.Run.ObservedState == domain.StatusWaitingApproval {
			fmt.Fprintf(
				config.Output,
				"run_converged worker=%s run=%s state=%s token=%d\n",
				config.WorkerID,
				config.RunID,
				result.State.Run.ObservedState,
				current.FencingToken,
			)
			return nil
		}
		if result.Command.Type == domain.CommandNoop {
			if err := waitContext(sessionCtx, config.PollInterval); err != nil {
				return err
			}
		}
	}
}

type leaseState struct {
	mu      sync.RWMutex
	current lease.Lease
}

func (state *leaseState) load() lease.Lease {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.current
}

func (state *leaseState) store(current lease.Lease) {
	state.mu.Lock()
	state.current = current
	state.mu.Unlock()
}

func leaseRenewalLoop(
	ctx context.Context,
	manager lease.Manager,
	session *leaseState,
	ttl time.Duration,
	interval time.Duration,
	errorsCh chan<- error,
	cancel context.CancelFunc,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewed, err := manager.Renew(ctx, session.load(), ttl)
			if err != nil {
				sendHeartbeatError(errorsCh, err)
				cancel()
				return
			}
			session.store(renewed)
		}
	}
}

func workerHeartbeatLoop(
	ctx context.Context,
	manager lease.Manager,
	workerID string,
	interval time.Duration,
	errorsCh chan<- error,
	cancel context.CancelFunc,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := manager.Heartbeat(ctx, workerID); err != nil {
				sendHeartbeatError(errorsCh, fmt.Errorf("worker heartbeat: %w", err))
				cancel()
				return
			}
		}
	}
}

func sendHeartbeatError(errorsCh chan<- error, err error) {
	select {
	case errorsCh <- err:
	default:
	}
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
