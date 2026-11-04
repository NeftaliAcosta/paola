package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type AgentRunner interface {
	Run(context.Context, string, string, string, string, string, chan<- string) (string, error)
}

type Runner struct{}

const defaultIdleTimeout = 30 * time.Minute

type idleTimeoutContextKey struct{}

// WithIdleTimeout configures the maximum period an agent may remain silent.
// Output from stdout or stderr resets the timer; active agents are not stopped.
func WithIdleTimeout(ctx context.Context, timeout time.Duration) context.Context {
	if timeout <= 0 {
		return ctx
	}
	return context.WithValue(ctx, idleTimeoutContextKey{}, timeout)
}

func idleTimeout(ctx context.Context) time.Duration {
	if timeout, ok := ctx.Value(idleTimeoutContextKey{}).(time.Duration); ok && timeout > 0 {
		return timeout
	}
	return defaultIdleTimeout
}

func (Runner) Run(ctx context.Context, root, cli, template, prompt, model string, logs chan<- string) (string, error) {
	if _, err := exec.LookPath(cli); err != nil {
		return "", fmt.Errorf("agent %q not found in PATH", cli)
	}
	modelArgs := ""
	if strings.TrimSpace(model) != "" {
		modelArgs = "--model " + shellQuote(strings.TrimSpace(model))
	}
	command := strings.ReplaceAll(template, "{{model_args}}", modelArgs)
	command = strings.ReplaceAll(command, "{{prompt}}", shellQuote(prompt))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	timeout := idleTimeout(ctx)
	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = root
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	var all strings.Builder
	var mu sync.Mutex
	var wg sync.WaitGroup
	var lastOutput atomic.Int64
	var timedOut atomic.Bool
	lastOutput.Store(time.Now().UnixNano())
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if time.Since(time.Unix(0, lastOutput.Load())) >= timeout {
					timedOut.Store(true)
					cancel()
					return
				}
			case <-runCtx.Done():
				return
			}
		}
	}()
	read := func(s *bufio.Scanner) {
		defer wg.Done()
		s.Buffer(make([]byte, 64*1024), 2*1024*1024)
		for s.Scan() {
			line := s.Text()
			lastOutput.Store(time.Now().UnixNano())
			mu.Lock()
			all.WriteString(line)
			all.WriteByte('\n')
			mu.Unlock()
			if logs != nil {
				select {
				case logs <- line:
				case <-runCtx.Done():
					return
				}
			}
		}
	}
	wg.Add(2)
	go read(bufio.NewScanner(out))
	go read(bufio.NewScanner(stderr))
	// Drain both pipes before Wait so large agent output cannot deadlock the child.
	wg.Wait()
	err = cmd.Wait()
	cancel()
	<-watchdogDone
	mu.Lock()
	result := all.String()
	mu.Unlock()
	if timedOut.Load() {
		return result, fmt.Errorf("agent %q produced no output for %s and was stopped; inspect the last log and retry", cli, timeout)
	}
	if err != nil && strings.TrimSpace(result) != "" {
		return result, fmt.Errorf("agent %q failed: %w: %s", cli, err, strings.TrimSpace(result))
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return result, fmt.Errorf("agent %q exited with code %d and produced no output; verify its authentication, model, permissions, and CLI configuration", cli, exitErr.ExitCode())
		}
		return result, fmt.Errorf("agent %q failed without output: %w", cli, err)
	}
	return result, err
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
