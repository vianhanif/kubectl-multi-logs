package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jedib0t/go-pretty/v6/progress"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

// ─── Helpers ─────────────────────────────────────────────────────────────────

const defaultOutput = "tail_multiple_logs_data.log"

var braille = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// activePW holds the progress.Writer currently rendering on screen so the
// signal handler can stop it cleanly before printing the Ctrl+C banner.
var (
	activePWMu sync.Mutex
	activePW   progress.Writer
)

// clearLine erases the current terminal line and moves cursor to column 0.
func clearLine() { fmt.Print("\r\033[K") }

// printSpinner overwrites the current line with a spinner (no newline).
func printSpinner(spin, msg string) {
	fmt.Printf("\r\033[K  %s  %s", text.FgYellow.Sprint(spin), msg)
}

// truncate shortens s to at most n bytes, appending "..." if cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ─── Domain types ────────────────────────────────────────────────────────────

// appPod associates a pod name with its owning app label.
type appPod struct{ app, pod string }

// podContainers holds the container list for a single pod.
type podContainers struct {
	app        string
	pod        string
	containers []string
}

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

// ─── Phase monitor (finding pods / fetching containers) ──────────────────────

// phaseItem carries the result of one parallel task once it finishes.
type phaseItem struct {
	label  string
	result string
	ok     bool
}

// newPW returns a configured progress.Writer ready to use (call go pw.Render()).
func newPW() progress.Writer {
	pw := progress.NewWriter()
	pw.SetAutoStop(false)
	pw.SetStyle(progress.StyleBlocks)
	pw.Style().Colors = progress.StyleColors{
		Message: text.Colors{},
		Error:   text.Colors{text.FgRed},
		Stats:   text.Colors{text.FgHiBlack},
		Time:    text.Colors{text.FgHiBlack},
		Tracker: text.Colors{text.FgHiBlack},
		Value:   text.Colors{text.FgHiBlack},
		Speed:   text.Colors{text.FgHiBlack},
	}
	pw.Style().Chars.Finished = "✔"
	pw.Style().Options.DoneString = ""
	pw.Style().Options.ErrorString = ""
	pw.Style().Visibility = progress.StyleVisibility{
		Time:           true,
		Tracker:        true,
		Value:          false,
		Percentage:     false,
		ETA:            false,
		ETAOverall:     false,
		Speed:          false,
		SpeedOverall:   false,
		TrackerOverall: false,
	}
	pw.SetTrackerPosition(progress.PositionRight)
	pw.SetUpdateFrequency(80 * time.Millisecond)
	return pw
}

// phaseMonitor shows a live per-item progress display for a batch of parallel
// tasks. labels must correspond 1-to-1 with items that will arrive on doneCh.
func phaseMonitor(labels []string, doneCh <-chan phaseItem) {
	pw := newPW()
	trackers := make(map[string]*progress.Tracker, len(labels))
	for _, label := range labels {
		t := &progress.Tracker{Message: "  " + label, Total: 0}
		pw.AppendTracker(t)
		trackers[label] = t
	}

	go pw.Render()

	for range labels {
		item := <-doneCh
		t := trackers[item.label]
		if item.ok {
			t.UpdateMessage(fmt.Sprintf("  %-50s %s", item.label, text.FgHiBlack.Sprint(item.result)))
			t.MarkAsDone()
		} else {
			t.UpdateMessage(fmt.Sprintf("  %-50s %s", item.label, text.FgRed.Sprint(item.result)))
			t.MarkAsErrored()
		}
	}
	pw.Stop()
}

// ─── Kubernetes helpers ───────────────────────────────────────────────────────

func kubectlArgs(namespace string, extra ...string) []string {
	args := extra
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return args
}

// getPodsForApp returns all pod names for the given app label.
func getPodsForApp(app, namespace string) ([]string, error) {
	args := kubectlArgs(namespace,
		"get", "pods",
		"-l", "app="+app,
		"--no-headers",
		"-o", "custom-columns=:metadata.name",
	)
	out, err := exec.Command("kubectl", args...).Output()
	if err != nil {
		return nil, err
	}
	var pods []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			pods = append(pods, line)
		}
	}
	return pods, nil
}

// getContainersForPod returns all container names in the given pod.
func getContainersForPod(pod, namespace string) ([]string, error) {
	args := kubectlArgs(namespace,
		"get", "pod", pod,
		"-o", "jsonpath={.spec.containers[*].name}",
	)
	out, err := exec.Command("kubectl", args...).Output()
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, nil
	}
	return strings.Fields(raw), nil
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

func (s *streamState) addLines(n int64)     { atomic.AddInt64(&s.count, n) }
func (s *streamState) markDone()            { atomic.StoreInt32(&s.done, 1) }
func (s *streamState) isDone() bool         { return atomic.LoadInt32(&s.done) == 1 }
func (s *streamState) lineCount() int64     { return atomic.LoadInt64(&s.count) }
func (s *streamState) setError(msg string)  { s.errMsg = msg }
func (s *streamState) isFailed() bool       { return s.errMsg != "" }
func (s *streamState) markTimedOut()        { atomic.StoreInt32(&s.timedOut, 1) }
func (s *streamState) isTimedOut() bool     { return atomic.LoadInt32(&s.timedOut) == 1 }

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
		text := scanner.Text()
		if cfg.grepPattern != "" && !matchesPattern(text, cfg.grepPattern) {
			continue
		}
		now := time.Now()
		if firstLine {
			st.startedAt = now
			firstLine = false
		}
		st.lastAt = now
		line := prefix + text + "\n"
		cfg.mu.Lock()
		cfg.outFile.WriteString(line) //nolint:errcheck
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

// matchesPattern reports whether text contains any of the |-separated patterns
// (case-insensitive).
func matchesPattern(text, pattern string) bool {
	lower := strings.ToLower(text)
	for _, pat := range strings.Split(pattern, "|") {
		if strings.Contains(lower, strings.ToLower(strings.TrimSpace(pat))) {
			return true
		}
	}
	return false
}

// ─── Display monitor (Phase 3) ────────────────────────────────────────────────

// displayMonitor uses a live go-pretty progress display to track every log
// stream. Trackers update with live line counts; finished streams are marked
// done (✔) or errored (✗). Blocks until all streams are done.
func displayMonitor(streams []*streamState, since string) {
	// Sort by app then pod for consistent ordering.
	sorted := make([]*streamState, len(streams))
	copy(sorted, streams)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].app != sorted[j].app {
			return sorted[i].app < sorted[j].app
		}
		return sorted[i].pod < sorted[j].pod
	})

	verb := "collected"
	if since == "" {
		verb = "following"
	}

	pw := newPW()

	type entry struct {
		st      *streamState
		tracker *progress.Tracker
	}
	entries := make([]entry, len(sorted))

	streamsLabel := func(st *streamState) string {
		return fmt.Sprintf("  [%s] %s › %s",
			text.FgCyan.Sprint(st.app), st.pod, st.container)
	}
	for i, st := range sorted {
		t := &progress.Tracker{Message: streamsLabel(st), Total: 0}
		pw.AppendTracker(t)
		entries[i] = entry{st, t}
	}

	// Register as the active writer so the signal handler can stop it.
	activePWMu.Lock()
	activePW = pw
	activePWMu.Unlock()
	defer func() {
		activePWMu.Lock()
		activePW = nil
		activePWMu.Unlock()
	}()

	go pw.Render()

	total := len(sorted)
	doneCount := 0
	finalized := make([]bool, total)
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	for doneCount < total {
		<-ticker.C
		for i, e := range entries {
			if finalized[i] {
				continue
			}
			base := streamsLabel(e.st)
			if e.st.isDone() {
				if e.st.isFailed() {
					e.tracker.UpdateMessage(base + "  " + text.FgRed.Sprint(truncate(e.st.errMsg, 60)))
					e.tracker.MarkAsErrored()
				} else if e.st.isTimedOut() {
					e.tracker.UpdateMessage(fmt.Sprintf("%s  %s",
						base,
						text.FgYellow.Sprintf("timed out · %d lines", e.st.lineCount()),
					))
					e.tracker.MarkAsDone()
				} else {
					e.tracker.UpdateMessage(fmt.Sprintf("%s  %s",
						base,
						text.FgHiBlack.Sprintf("%s · %d lines", verb, e.st.lineCount()),
					))
					e.tracker.MarkAsDone()
				}
				finalized[i] = true
				doneCount++
			} else {
				e.tracker.UpdateMessage(base + "  " + text.FgHiBlack.Sprint(e.st.statusLabel()))
			}
		}
	}
	pw.Stop()
}

// ─── Summary ─────────────────────────────────────────────────────────────────

// longestCommonPrefix returns the longest common prefix of all strings in ss.
func longestCommonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	prefix := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, prefix) {
			prefix = prefix[:len(prefix)-1]
			if prefix == "" {
				return ""
			}
		}
	}
	return prefix
}

// summaryTableStyle is the go-pretty table style used for the summary.
var summaryTableStyle = table.Style{
	Name: "Summary",
	Box:  table.StyleBoxRounded,
	Color: table.ColorOptions{
		Header:    text.Colors{text.Bold},
		Footer:    text.Colors{text.Bold},
		Border:    text.Colors{text.FgHiBlack},
		Separator: text.Colors{text.FgHiBlack},
	},
	Format: table.FormatOptions{
		Header: text.FormatUpper,
		Footer: text.FormatDefault,
	},
	Options: table.Options{
		DrawBorder:      true,
		SeparateColumns: true,
		SeparateHeader:  true,
		SeparateRows:    false,
	},
}

// printSummary prints a per-app breakdown with per-container detail,
// including line counts and first/last log timestamps, using go-pretty/table.
func printSummary(streams []*streamState) {
	if len(streams) == 0 {
		return
	}

	const tsLayout = "15:04:05"

	type appGroup struct {
		pods       map[string]bool
		containers []*streamState
		lines      int64
		errors     int
		timedOuts  int
	}

	groups := map[string]*appGroup{}
	var appOrder []string
	for _, st := range streams {
		if _, ok := groups[st.app]; !ok {
			groups[st.app] = &appGroup{pods: map[string]bool{}}
			appOrder = append(appOrder, st.app)
		}
		g := groups[st.app]
		g.pods[st.pod] = true
		g.containers = append(g.containers, st)
		g.lines += st.lineCount()
		if st.isFailed() {
			g.errors++
		}
		if st.isTimedOut() {
			g.timedOuts++
		}
	}
	sort.Strings(appOrder)

	for _, g := range groups {
		sort.Slice(g.containers, func(i, j int) bool {
			if g.containers[i].pod != g.containers[j].pod {
				return g.containers[i].pod < g.containers[j].pod
			}
			return g.containers[i].container < g.containers[j].container
		})
	}

	totalPods, totalContainers, totalLines := 0, 0, int64(0)
	for _, g := range groups {
		totalPods += len(g.pods)
		totalContainers += len(g.containers)
		totalLines += g.lines
	}

	fmt.Println()
	fmt.Println(text.Bold.Sprint("── Summary"))

	for _, app := range appOrder {
		g := groups[app]

		// Strip the common pod-name prefix to keep POD column narrow.
		allPodNames := make([]string, len(g.containers))
		for i, st := range g.containers {
			allPodNames[i] = st.pod
		}
		podPrefix := longestCommonPrefix(allPodNames)
		if len(podPrefix) > 0 && len(podPrefix) >= len(allPodNames[0])-1 {
			podPrefix = ""
		}

		// App section header.
		errSuffix := ""
		if g.errors > 0 {
			errSuffix = "  " + text.FgRed.Sprintf("(%d err)", g.errors)
		}
		timeoutSuffix := ""
		if g.timedOuts > 0 {
			timeoutSuffix = "  " + text.FgYellow.Sprintf("(%d timed out)", g.timedOuts)
		}
		fmt.Printf("\n  %s  %s%s%s\n",
			text.Bold.Sprint(app),
			text.FgHiBlack.Sprintf("%d pod(s) · %d stream(s) · %d lines", len(g.pods), len(g.containers), g.lines),
			errSuffix,
			timeoutSuffix,
		)
		if podPrefix != "" {
			fmt.Printf("  %s\n", text.FgHiBlack.Sprintf("pod prefix: %s", podPrefix))
		}

		// Per-app table.
		t := table.NewWriter()
		t.SetOutputMirror(os.Stdout)
		t.SetStyle(summaryTableStyle)
		t.SetColumnConfigs([]table.ColumnConfig{
			{Number: 1, Align: text.AlignLeft},                                        // icon
			{Number: 2, Align: text.AlignLeft, Colors: text.Colors{text.FgCyan}},      // pod
			{Number: 3, Align: text.AlignLeft, Colors: text.Colors{text.FgCyan}},      // container
			{Number: 4, Align: text.AlignRight, Colors: text.Colors{text.FgHiWhite}},  // lines
			{Number: 5, Align: text.AlignCenter, Colors: text.Colors{text.FgHiBlack}}, // time range
		})
		t.AppendHeader(table.Row{" ", "POD", "CONTAINER", "LINES", "TIME RANGE"})

		for i, st := range g.containers {
			shortPod := strings.TrimPrefix(allPodNames[i], podPrefix)
			var icon string
			if st.isFailed() {
				icon = text.FgRed.Sprint("✗")
			} else if st.isTimedOut() {
				icon = text.FgYellow.Sprint("⏱")
			} else {
				icon = text.FgGreen.Sprint("✔")
			}
			timeRange := ""
			if !st.startedAt.IsZero() {
				timeRange = fmt.Sprintf("%s → %s",
					st.startedAt.Format(tsLayout), st.lastAt.Format(tsLayout))
				if st.isTimedOut() {
					timeRange += " (cut)"
				}
			} else if st.isFailed() {
				timeRange = truncate(st.errMsg, 30)
			}
			t.AppendRow(table.Row{icon, shortPod, st.container, st.lineCount(), timeRange})
		}
		t.Render()
	}

	// Grand total table.
	fmt.Println()
	tot := table.NewWriter()
	tot.SetOutputMirror(os.Stdout)
	tot.SetStyle(summaryTableStyle)
	tot.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft, Colors: text.Colors{text.Bold}},
		{Number: 2, Align: text.AlignRight, Colors: text.Colors{text.Bold}},
		{Number: 3, Align: text.AlignRight, Colors: text.Colors{text.Bold}},
		{Number: 4, Align: text.AlignRight, Colors: text.Colors{text.Bold}},
	})
	tot.AppendHeader(table.Row{"TOTAL", "PODS", "STREAMS", "LINES"})
	tot.AppendRow(table.Row{"all apps", totalPods, totalContainers, totalLines})
	tot.Render()
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	var (
		namespace     = flag.String("n", "", "Kubernetes namespace")
		since         = flag.String("s", "", "Show logs since (e.g. 10m, 1h)")
		grepPattern   = flag.String("g", "", "Filter log lines (case-insensitive, supports | for multiple patterns)")
		errorsOnly    = flag.Bool("e", false, "Filter for ERROR/WARN/Exception/failed/error")
		outputFile    = flag.String("o", defaultOutput, "Output file name (-o alone uses the default)")
		streamTimeout = flag.Duration("T", 2*time.Minute, "Per-stream timeout in collect mode (-s); 0 = no limit")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [-n namespace] [-s since] [-T timeout] [-g pattern] [-e] [-o [output_file]] <app1> <app2> ...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	// Bare -o with no value: default is already set by flag package; nothing to do.
	// If the user passed -o without a filename the flag package would have errored,
	// so we handle it by scanning for the bare token before Parse.
	for i, arg := range os.Args[1:] {
		if arg == "-o" {
			os.Args[i+1] = "-o=" + defaultOutput
			// Re-parse with the corrected arg.
			flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
			namespace = flag.String("n", "", "Kubernetes namespace")
			since = flag.String("s", "", "Show logs since (e.g. 10m, 1h)")
			grepPattern = flag.String("g", "", "Filter log lines (case-insensitive, supports | for multiple patterns)")
			errorsOnly = flag.Bool("e", false, "Filter for ERROR/WARN/Exception/failed/error")
			outputFile = flag.String("o", defaultOutput, "Output file name")
			streamTimeout = flag.Duration("T", 2*time.Minute, "Per-stream timeout in collect mode (-s); 0 = no limit")
			flag.Parse()
			break
		}
	}

	apps := flag.Args()
	if len(apps) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	pattern := buildPattern(*grepPattern, *errorsOnly)

	scriptDir, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	outPath := filepath.Join(scriptDir, *outputFile)

	// ── Phase 1 & 2 ─────────────────────────────────────────────────────────
	appPods := runPhase1(apps, *namespace)
	if len(appPods) == 0 {
		fmt.Fprintln(os.Stderr, "No pods found for the specified apps. Exiting.")
		os.Exit(1)
	}
	allPodContainers := runPhase2(appPods, *namespace)

	// ── Open output file ────────────────────────────────────────────────────
	outFile, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot open output file %s: %v\n", outPath, err)
		os.Exit(1)
	}
	defer outFile.Close()

	var fileMu sync.Mutex
	cfg := streamConfig{
		follow:        *since == "",
		since:         *since,
		namespace:     *namespace,
		grepPattern:   pattern,
		outFile:       outFile,
		mu:            &fileMu,
		streamTimeout: *streamTimeout,
	}

	if cfg.since != "" {
		fmt.Printf("Showing historical logs from %s to now for %d pods and their containers.\n", cfg.since, len(appPods))
	} else {
		fmt.Printf("Starting log tailing for %d pods and their containers.\n", len(appPods))
	}
	fmt.Printf("Logs are being saved to: %s\n", outPath)
	fmt.Println("Press Ctrl+C to stop all log streams")
	fmt.Println("----------------------------------------")

	// ── Signal handler ──────────────────────────────────────────────────────
	var streamsStore atomic.Value
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		// Stop any live progress writer so its render loop doesn't race with
		// the Ctrl+C banner we're about to print.
		activePWMu.Lock()
		pw := activePW
		activePWMu.Unlock()
		if pw != nil {
			pw.Stop()
		}
		clearLine()
		fmt.Printf("\n%s\n", text.FgYellow.Sprint("Ctrl+C received — stopping all log streams..."))
		outFile.Close()
		if v := streamsStore.Load(); v != nil {
			printSummary(v.([]*streamState))
		}
		os.Exit(0)
	}()

	// ── Phase 3: Stream logs ────────────────────────────────────────────────
	streams, wg := launchStreams(allPodContainers, cfg)
	streamsStore.Store(streams)

	displayMonitor(streams, cfg.since)
	wg.Wait()

	printSummary(streams)
	if cfg.since != "" {
		fmt.Printf("Historical logs collection completed. Logs saved to: %s\n", outPath)
	}
}

// buildPattern merges the user grep pattern with the errors-only terms.
func buildPattern(grepPattern string, errorsOnly bool) string {
	if !errorsOnly {
		return grepPattern
	}
	const errorTerms = "ERROR|WARN|Exception|failed|error"
	if grepPattern != "" {
		return grepPattern + "|" + errorTerms
	}
	return errorTerms
}

// runPhase1 finds all pods for the given apps in parallel and returns
// the (app, pod) pairs. The progress is displayed via phaseMonitor.
func runPhase1(apps []string, namespace string) []appPod {
	var mu sync.Mutex
	var result []appPod

	fmt.Println(text.Bold.Sprint("Finding pods for apps..."))
	doneCh := make(chan phaseItem, len(apps))
	var wg sync.WaitGroup
	for _, app := range apps {
		app := app
		wg.Add(1)
		go func() {
			defer wg.Done()
			pods, err := getPodsForApp(app, namespace)
			if err != nil || len(pods) == 0 {
				doneCh <- phaseItem{label: app, result: "no pods found", ok: false}
				return
			}
			mu.Lock()
			for _, p := range pods {
				result = append(result, appPod{app, p})
			}
			mu.Unlock()
			doneCh <- phaseItem{label: app, result: fmt.Sprintf("%d pod(s)", len(pods)), ok: true}
		}()
	}
	go func() { wg.Wait(); close(doneCh) }()
	phaseMonitor(apps, doneCh)
	return result
}

// runPhase2 fetches containers for every pod in parallel and returns
// the full pod→containers mapping. The progress is displayed via phaseMonitor.
func runPhase2(pods []appPod, namespace string) []podContainers {
	var mu sync.Mutex
	var result []podContainers

	fmt.Println(text.Bold.Sprint("Fetching containers for pods..."))
	doneCh := make(chan phaseItem, len(pods))
	var wg sync.WaitGroup
	for _, ap := range pods {
		ap := ap
		wg.Add(1)
		go func() {
			defer wg.Done()
			containers, err := getContainersForPod(ap.pod, namespace)
			if err != nil || len(containers) == 0 {
				doneCh <- phaseItem{label: ap.pod, result: "no containers", ok: false}
				return
			}
			mu.Lock()
			result = append(result, podContainers{ap.app, ap.pod, containers})
			mu.Unlock()
			doneCh <- phaseItem{label: ap.pod, result: fmt.Sprintf("%d container(s)", len(containers)), ok: true}
		}()
	}
	go func() { wg.Wait(); close(doneCh) }()
	podLabels := make([]string, len(pods))
	for i, ap := range pods {
		podLabels[i] = ap.pod
	}
	phaseMonitor(podLabels, doneCh)
	return result
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
