//go:build integration || chaos

package controller_test

import (
	"bytes"
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"
)

const workerTestStartupTimeout = 5 * time.Second

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *synchronizedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *synchronizedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

type testWorkerProcess struct {
	command *exec.Cmd
	output  *synchronizedBuffer
	done    chan struct{}
	waitMu  sync.Mutex
	waitErr error
}

func startTestWorkerProcess(
	t *testing.T,
	command *exec.Cmd,
) (*testWorkerProcess, *synchronizedBuffer) {
	t.Helper()
	output := &synchronizedBuffer{}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatalf("start Worker: %v", err)
	}
	process := &testWorkerProcess{
		command: command,
		output:  output,
		done:    make(chan struct{}),
	}
	go func() {
		waitErr := command.Wait()
		process.waitMu.Lock()
		process.waitErr = waitErr
		process.waitMu.Unlock()
		close(process.done)
	}()
	t.Cleanup(func() {
		select {
		case <-process.done:
			return
		default:
			_ = command.Process.Kill()
			<-process.done
		}
	})
	return process, output
}

func (process *testWorkerProcess) wait() error {
	<-process.done
	process.waitMu.Lock()
	defer process.waitMu.Unlock()
	return process.waitErr
}

func (process *testWorkerProcess) diagnostics(lastDatabaseError error) string {
	processState := "running"
	var waitErr error
	select {
	case <-process.done:
		if process.command.ProcessState != nil {
			processState = process.command.ProcessState.String()
		} else {
			processState = "exited-without-process-state"
		}
		process.waitMu.Lock()
		waitErr = process.waitErr
		process.waitMu.Unlock()
	default:
	}
	return fmt.Sprintf(
		"pid=%d process_state=%s wait_error=%v last_database_error=%v output=%q",
		process.command.Process.Pid,
		processState,
		waitErr,
		lastDatabaseError,
		process.output.String(),
	)
}
