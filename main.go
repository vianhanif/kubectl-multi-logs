package main

import (
	"bufio"
	"bytes"
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

	"golang.org/x/term"
)

// ─── ANSI helpers ────────────────────────────────────────────────────────────

const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiClearL = "\033[K"
	ansiRed    = "\033[31m"

	defaultOutput = "tail_multiple_logs_data.log"
)

var braille = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// appColorPalette provides distinct colors cycled per-app in the display.
var appColorPalette = []string{
	"\033[34m", // blue
	"\033[35m", // magenta
	"\033[33m", // yellow
	"\033[96m", // bright cyan
	"\033[95m", // bright magenta
	"\033[94m", // bright blue
	"\033[93m", // bright yellow
	"\033[92m", // bright green
	"\033[91m", // bright red
	"\033[31m", // red
}

func termWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	return 80
}

// printPermanent erases the spinner line and prints a permanent ✔ line.
func printPermanent(line string) {
	fmt.Printf("\r%s%s\n", ansiClearL, line)
}

// printSpinner overwrites the current line with a spinner status (no newline).
func printSpinner(spin, msg string) {
	fmt.Printf("\r%s  %s%s%s  %s%s", ansiClearL, ansiYellow, spin, ansiReset, msg, ansiClearL)
}

// rightPad returns a string padded between left and right to fill `width` columns.
func rightPad(left, right string, width int) string {
	pad := width - len(left) - len(right)
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
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
	follow      bool
	since       string
	namespace   string
	grepPattern string
	outFile     *os.File
	mu          *sync.Mutex
}

// ─── Phase monitor (finding pods / fetching containers) ──────────────────────

// phaseItem carries the result of one parallel task once it finishes.
type phaseItem struct {
	label  string
	result string
}

// printPhaseItem prints a single completed phase item permanently.
func printPhaseItem(item phaseItem) {
	cols := termWidth()
	leftVis := fmt.Sprintf("  ✔  %s", item.label)
	dimResult := fmt.Sprintf("%s%s%s", ansiDim, item.result, ansiReset)
	pad := cols - len(leftVis) - len(item.result)
	if pad < 1 {
		pad = 1
	}
	printPermanent(fmt.Sprintf("  %s✔%s  %s%s%s",
		ansiGreen, ansiReset, item.label, strings.Repeat(" ", pad), dimResult))
}

// phaseMonitor prints items permanently as they arrive on doneCh, with a
// live spinner line at the bottom. Blocks until all `total` items are printed.
func phaseMonitor(total int, doneCh <-chan phaseItem) {
	printed := 0
	spinIdx := 0
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	// drain flushes any items already in the channel buffer.
	drain := func() {
		for {
			select {
			case item := <-doneCh:
				printPhaseItem(item)
				printed++
			default:
				return
			}
		}
	}

	for printed < total {
		select {
		case item := <-doneCh:
			printPhaseItem(item)
			printed++
		case <-ticker.C:
			drain()
			spin := braille[spinIdx%len(braille)]
			spinIdx++
			printSpinner(spin, fmt.Sprintf("Waiting for %d more...", total-printed))
		}
	}
	fmt.Printf("\r%s", ansiClearL) // clear spinner line
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
	count               int64  // updated atomically
	done                int32  // 1 when kubectl exits, set atomically
	errMsg              string // written once before markDone; safe to read after isDone
}

func (s *streamState) addLines(n int64)    { atomic.AddInt64(&s.count, n) }
func (s *streamState) markDone()           { atomic.StoreInt32(&s.done, 1) }
func (s *streamState) isDone() bool        { return atomic.LoadInt32(&s.done) == 1 }
func (s *streamState) lineCount() int64    { return atomic.LoadInt64(&s.count) }
func (s *streamState) setError(msg string) { s.errMsg = msg }
func (s *streamState) isFailed() bool      { return s.errMsg != "" }

// ─── Log streaming ────────────────────────────────────────────────────────────

func streamLogs(st *streamState, cfg streamConfig) {
	defer st.markDone()

	args := kubectlArgs(cfg.namespace, "logs")
	if cfg.follow {
		args = append(args, "-f")
	}
	if cfg.since != "" {
		args = append(args, "--since="+cfg.since)
	}
	args = append(args, st.pod, "-c", st.container)

	var stderrBuf bytes.Buffer
	cmd := exec.Command("kubectl", args...)
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
	for scanner.Scan() {
		text := scanner.Text()
		if cfg.grepPattern != "" && !matchesPattern(text, cfg.grepPattern) {
			continue
		}
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

// displayMonitor prints log-stream progress in scroll-and-advance style,
// grouped by app. Blocks until all streams are done.
func displayMonitor(streams []*streamState, since string) {
	// Sort by app then pod for consistent grouping
	sorted := make([]*streamState, len(streams))
	copy(sorted, streams)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].app != sorted[j].app {
			return sorted[i].app < sorted[j].app
		}
		return sorted[i].pod < sorted[j].pod
	})

	// Assign a consistent color to each app (in sorted order)
	appColorMap := map[string]string{}
	colorIdx := 0
	for _, st := range sorted {
		if _, ok := appColorMap[st.app]; !ok {
			appColorMap[st.app] = appColorPalette[colorIdx%len(appColorPalette)]
			colorIdx++
		}
	}

	total := len(sorted)
	printedApp := map[string]bool{}
	printedStream := make([]bool, total)
	doneCount := 0
	spinIdx := 0
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	verb := "Following "
	if since != "" {
		verb = "Collecting"
	}

	printReady := func() {
		for i, st := range sorted {
			if printedStream[i] || !st.isDone() {
				continue
			}
			appColor := appColorMap[st.app]
			// Print app header once, in the app's color
			if !printedApp[st.app] {
				printPermanent(fmt.Sprintf("  %s%s%s%s", appColor, ansiBold, st.app, ansiReset))
				printedApp[st.app] = true
			}
			if st.isFailed() {
				printPermanent(fmt.Sprintf(
					"    %s✗%s [%s%s%s] %s  %s%s%s",
					ansiRed, ansiReset,
					appColor, st.pod, ansiReset,
					st.container,
					ansiDim, truncate(st.errMsg, 60), ansiReset,
				))
			} else {
				cols := termWidth()
				leftVis := fmt.Sprintf("    ✔ [%s] %s", st.pod, st.container)
				rightVis := fmt.Sprintf("Collected   %d lines", st.lineCount())
				if since == "" {
					rightVis = fmt.Sprintf("Following   %d lines", st.lineCount())
				}
				pad := cols - len(leftVis) - len(rightVis)
				if pad < 1 {
					pad = 1
				}
				printPermanent(fmt.Sprintf(
					"    %s✔%s [%s%s%s] %s%s%s%s%s",
					ansiGreen, ansiReset,
					appColor, st.pod, ansiReset,
					st.container,
					strings.Repeat(" ", pad),
					ansiDim, rightVis, ansiReset,
				))
			}
			printedStream[i] = true
			doneCount++
		}
	}

	for doneCount < total {
		select {
		case <-ticker.C:
			printReady()
			if doneCount < total {
				spin := braille[spinIdx%len(braille)]
				spinIdx++
				printSpinner(spin, fmt.Sprintf("%s logs... (%d/%d streams done)", verb, doneCount, total))
			}
		}
	}
	fmt.Printf("\r%s", ansiClearL)
}

// ─── Summary ─────────────────────────────────────────────────────────────────

// printSummary prints a per-app table of pods, streams, and lines collected.
func printSummary(streams []*streamState) {
	if len(streams) == 0 {
		return
	}

	type appStat struct {
		pods       map[string]bool
		containers int
		lines      int64
		errors     int
	}
	stats := map[string]*appStat{}
	var appOrder []string

	for _, st := range streams {
		if _, ok := stats[st.app]; !ok {
			stats[st.app] = &appStat{pods: map[string]bool{}}
			appOrder = append(appOrder, st.app)
		}
		s := stats[st.app]
		s.pods[st.pod] = true
		s.containers++
		s.lines += st.lineCount()
		if st.isFailed() {
			s.errors++
		}
	}
	sort.Strings(appOrder)

	totalPods, totalContainers, totalLines := 0, 0, int64(0)
	for _, s := range stats {
		totalPods += len(s.pods)
		totalContainers += s.containers
		totalLines += s.lines
	}

	sep := "  " + strings.Repeat("─", 66)
	fmt.Printf("\n%s── Summary ──────────────────────────────────────────────────────%s\n", ansiBold, ansiReset)
	fmt.Printf("  %-38s  %4s  %8s  %10s\n", "App", "Pods", "Streams", "Lines")
	fmt.Println(sep)
	for _, app := range appOrder {
		s := stats[app]
		errMark := ""
		if s.errors > 0 {
			errMark = fmt.Sprintf("  %s(%d err)%s", ansiRed, s.errors, ansiReset)
		}
		fmt.Printf("  %-38s  %4d  %8d  %10d%s\n",
			app, len(s.pods), s.containers, s.lines, errMark)
	}
	fmt.Println(sep)
	fmt.Printf("  %-38s  %4d  %8d  %10d\n", "TOTAL", totalPods, totalContainers, totalLines)
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	var (
		namespace   = flag.String("n", "", "Kubernetes namespace")
		since       = flag.String("s", "", "Show logs since (e.g. 10m, 1h)")
		grepPattern = flag.String("g", "", "Filter log lines (case-insensitive, supports | for multiple patterns)")
		errorsOnly  = flag.Bool("e", false, "Filter for ERROR/WARN/Exception/failed/error")
		outputFile  = flag.String("o", defaultOutput, "Output file name (-o alone uses the default)")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [-n namespace] [-s since] [-g pattern] [-e] [-o [output_file]] <app1> <app2> ...\n", os.Args[0])
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
		follow:      *since == "",
		since:       *since,
		namespace:   *namespace,
		grepPattern: pattern,
		outFile:     outFile,
		mu:          &fileMu,
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
		fmt.Printf("\r%s\n%sCtrl+C received — stopping all log streams...%s\n",
			ansiClearL, ansiYellow, ansiReset)
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

	fmt.Println("Finding pods for apps...")
	doneCh := make(chan phaseItem, len(apps))
	var wg sync.WaitGroup
	for _, app := range apps {
		app := app
		wg.Add(1)
		go func() {
			defer wg.Done()
			pods, err := getPodsForApp(app, namespace)
			if err != nil || len(pods) == 0 {
				doneCh <- phaseItem{label: app, result: "no pods found"}
				return
			}
			mu.Lock()
			for _, p := range pods {
				result = append(result, appPod{app, p})
			}
			mu.Unlock()
			doneCh <- phaseItem{label: app, result: fmt.Sprintf("%d pod(s)", len(pods))}
		}()
	}
	go func() { wg.Wait(); close(doneCh) }()
	phaseMonitor(len(apps), doneCh)
	return result
}

// runPhase2 fetches containers for every pod in parallel and returns
// the full pod→containers mapping. The progress is displayed via phaseMonitor.
func runPhase2(pods []appPod, namespace string) []podContainers {
	var mu sync.Mutex
	var result []podContainers

	fmt.Println("Fetching containers for pods...")
	doneCh := make(chan phaseItem, len(pods))
	var wg sync.WaitGroup
	for _, ap := range pods {
		ap := ap
		wg.Add(1)
		go func() {
			defer wg.Done()
			containers, err := getContainersForPod(ap.pod, namespace)
			if err != nil || len(containers) == 0 {
				doneCh <- phaseItem{label: ap.pod, result: "no containers"}
				return
			}
			mu.Lock()
			result = append(result, podContainers{ap.app, ap.pod, containers})
			mu.Unlock()
			doneCh <- phaseItem{label: ap.pod, result: fmt.Sprintf("%d container(s)", len(containers))}
		}()
	}
	go func() { wg.Wait(); close(doneCh) }()
	phaseMonitor(len(pods), doneCh)
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
				fmt.Printf("\r%s", ansiClearL)
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
