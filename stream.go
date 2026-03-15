package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// streamConfig holds the shared, read-only parameters for every log stream.
type streamConfig struct {
	follow        bool
	since         string
	namespace     string
	grepPattern   string
	outFile       *os.File
	mu            *sync.Mutex
	streamTimeout time.Duration // 0 = unlimited; only applied in collect mode
}

// ─── Stream state ─────────────────────────────────────────────────────────────

// streamState tracks live stats for one pod/container log stream.
type streamState struct {
	app, pod, container string
	count               int64     // updated atomically
	done                int32     // 1 when kubectl exits, set atomically
	timedOut            int32     // 1 if stream was cut by per-stream timeout
	errMsg              string    // written once before markDone; safe to read after isDone
	launchedAt          time.Time // wall time when goroutine started
	startedAt           time.Time // wall time when first log line received; zero if no lines
	lastAt              time.Time // wall time when last log line received
}

func (s *streamState) addLines(n int64)    { atomic.AddInt64(&s.count, n) }
func (s *streamState) markDone()           { atomic.StoreInt32(&s.done, 1) }
func (s *streamState) isDone() bool        { return atomic.LoadInt32(&s.done) == 1 }
func (s *streamState) lineCount() int64    { return atomic.LoadInt64(&s.count) }
func (s *streamState) setError(msg string) { s.errMsg = msg }
func (s *streamState) isFailed() bool      { return s.errMsg != "" }
func (s *streamState) markTimedOut()       { atomic.StoreInt32(&s.timedOut, 1) }
func (s *streamState) isTimedOut() bool    { return atomic.LoadInt32(&s.timedOut) == 1 }

// statusLabel returns a short human-readable status for a pending stream.
func (s *streamState) statusLabel() string {
	elapsed := time.Since(s.launchedAt).Round(time.Second)
	if !s.startedAt.IsZero() {
		return fmt.Sprintf("streaming · %d lines · %s", s.lineCount(), elapsed)
	}
	return fmt.Sprintf("waiting for logs… (%s)", elapsed)
}

// ─── Log streaming ────────────────────────────────────────────────────────────

func streamLogs(st *streamState, cfg streamConfig) {
	defer st.markDone()
	st.launchedAt = time.Now()

	ctx := context.Background()
	var cancel context.CancelFunc = func() {}
	if !cfg.follow && cfg.streamTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, cfg.streamTimeout)
	}
	defer cancel()

	args := kubectlArgs(cfg.namespace, "logs")
	if cfg.follow {
		args = append(args, "-f")
	}
	if cfg.since != "" {
		args = append(args, "--since="+cfg.since)
	}
	args = append(args, st.pod, "-c", st.container)

	var stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		st.setError(fmt.Sprintf("stdout pipe: %v", err))
		return
	}
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		st.setError(fmt.Sprintf("start: %v", err))
		return
	}

	prefix := fmt.Sprintf("[%s:%s] ", st.pod, st.container)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var batch int64
	firstLine := true
	for scanner.Scan() {
		line := scanner.Text()
		if cfg.grepPattern != "" && !matchesPattern(line, cfg.grepPattern) {
			continue
		}
		now := time.Now()
		if firstLine {
			st.startedAt = now
			firstLine = false
		}
		st.lastAt = now
		out := prefix + line + "\n"
		cfg.mu.Lock()
		cfg.outFile.WriteString(out) //nolint:errcheck
		cfg.mu.Unlock()
		batch++
		if batch >= 50 {
			st.addLines(batch)
			batch = 0
		}
	}
	if batch > 0 {
		st.addLines(batch)
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			// Timeout fired — keep whatever was collected, not an error.
			st.markTimedOut()
			return
		}
		msg := strings.TrimSpace(stderrBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		st.setError(truncate(msg, 120))
	}
}

// matchesPattern reports whether line contains any of the |-separated patterns
// (case-insensitive).
func matchesPattern(line, pattern string) bool {
	lower := strings.ToLower(line)
	for _, pat := range strings.Split(pattern, "|") {
		if strings.Contains(lower, strings.ToLower(strings.TrimSpace(pat))) {
			return true
		}
	}
	return false
}

// launchStreams starts one goroutine per pod/container, preceded by a prep
// spinner. Returns the stream list and the WaitGroup for callers to Wait on.
func launchStreams(allPodContainers []podContainers, cfg streamConfig) ([]*streamState, *sync.WaitGroup) {
	var streams []*streamState
	var wg sync.WaitGroup

	prepDone := make(chan struct{})
	go func() {
		i := 0
		for {
			select {
			case <-prepDone:
				clearLine()
				return
			case <-time.After(100 * time.Millisecond):
				printSpinner(braille[i%len(braille)], "Preparing streams...")
				i++
			}
		}
	}()

	for _, pc := range allPodContainers {
		for _, container := range pc.containers {
			st := &streamState{app: pc.app, pod: pc.pod, container: container}
			streams = append(streams, st)
			wg.Add(1)
			go func(st *streamState) {
				defer wg.Done()
				streamLogs(st, cfg)
			}(st)
		}
	}

	close(prepDone)
	return streams, &wg
}
