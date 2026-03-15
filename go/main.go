package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
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
	ansiCyan   = "\033[36m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiClearL = "\033[K"
)

var braille = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

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

// ─── Phase monitor (finding pods / fetching containers) ──────────────────────

// phaseItem carries the result of one parallel task once it finishes.
type phaseItem struct {
	label  string
	result string
}

// phaseMonitor prints items permanently as they arrive on doneCh, with a
// live spinner line at the bottom. Blocks until all `total` items are printed.
func phaseMonitor(total int, doneCh <-chan phaseItem) {
	printed := 0
	spinIdx := 0
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	// Drain any items that arrived before the first tick.
	drain := func() {
		for {
			select {
			case item := <-doneCh:
				cols := termWidth()
				leftVis := fmt.Sprintf("  ✔  %s", item.label)
				dimResult := fmt.Sprintf("%s%s%s", ansiDim, item.result, ansiReset)
				line := rightPad(leftVis, dimResult, cols)
				printPermanent(fmt.Sprintf("  %s✔%s  %s", ansiGreen, ansiReset, line[5:]))
				printed++
			default:
				return
			}
		}
	}

	for printed < total {
		select {
		case item := <-doneCh:
			cols := termWidth()
			leftVis := fmt.Sprintf("  ✔  %s", item.label)
			dimResult := fmt.Sprintf("%s%s%s", ansiDim, item.result, ansiReset)
			pad := cols - len(leftVis) - len(item.result)
			if pad < 1 {
				pad = 1
			}
			printPermanent(fmt.Sprintf("  %s✔%s  %s%s%s", ansiGreen, ansiReset, item.label, strings.Repeat(" ", pad), dimResult))
			printed++
		case <-ticker.C:
			drain()
			remaining := total - printed
			spin := braille[spinIdx%len(braille)]
			spinIdx++
			printSpinner(spin, fmt.Sprintf("Waiting for %d more...", remaining))
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
	count               int64 // updated atomically
	done                int32 // 1 when kubectl exits, set atomically
}

func (s *streamState) addLines(n int64) { atomic.AddInt64(&s.count, n) }
func (s *streamState) markDone()        { atomic.StoreInt32(&s.done, 1) }
func (s *streamState) isDone() bool     { return atomic.LoadInt32(&s.done) == 1 }
func (s *streamState) lineCount() int64 { return atomic.LoadInt64(&s.count) }

// ─── Log streaming ────────────────────────────────────────────────────────────

func streamLogs(st *streamState, follow bool, since, namespace, grepPattern string, outFile *os.File, mu *sync.Mutex) {
	defer st.markDone()

	args := kubectlArgs(namespace, "logs")
	if follow {
		args = append(args, "-f")
	}
	if since != "" {
		args = append(args, "--since="+since)
	}
	args = append(args, st.pod, "-c", st.container)

	cmd := exec.Command("kubectl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return
	}

	prefix := fmt.Sprintf("[%s:%s] ", st.pod, st.container)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var batch int64
	for scanner.Scan() {
		text := scanner.Text()

		// Apply grep filter if specified
		if grepPattern != "" && !strings.Contains(strings.ToLower(text), strings.ToLower(grepPattern)) {
			// Check each pipe-separated pattern
			matched := false
			for _, pat := range strings.Split(grepPattern, "|") {
				if strings.Contains(strings.ToLower(text), strings.ToLower(strings.TrimSpace(pat))) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		line := prefix + text + "\n"
		mu.Lock()
		outFile.WriteString(line) //nolint:errcheck
		mu.Unlock()
		batch++
		if batch >= 50 {
			st.addLines(batch)
			batch = 0
		}
	}
	if batch > 0 {
		st.addLines(batch)
	}

	cmd.Wait() //nolint:errcheck
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
			// Print app header once
			if !printedApp[st.app] {
				printPermanent(fmt.Sprintf("  %s%s%s", ansiBold, st.app, ansiReset))
				printedApp[st.app] = true
			}
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
				ansiCyan, st.pod, ansiReset,
				st.container,
				strings.Repeat(" ", pad),
				ansiDim, rightVis, ansiReset,
			))
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

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	var (
		namespace   = flag.String("n", "", "Kubernetes namespace")
		since       = flag.String("s", "", "Show logs since (e.g. 10m, 1h)")
		grepPattern = flag.String("g", "", "Filter log lines (case-insensitive, supports | for multiple patterns)")
		errorsOnly  = flag.Bool("e", false, "Filter for ERROR/WARN/Exception/failed/error")
		outputFile  = flag.String("o", "tail_multiple_logs_data.log", "Output file name")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [-n namespace] [-s since] [-g pattern] [-e] [-o output_file] <app1> <app2> ...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	apps := flag.Args()
	if len(apps) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	// Build grep pattern
	pattern := *grepPattern
	if *errorsOnly {
		errorTerms := "ERROR|WARN|Exception|failed|error"
		if pattern != "" {
			pattern = pattern + "|" + errorTerms
		} else {
			pattern = errorTerms
		}
	}

	// Resolve output file path relative to binary location
	scriptDir, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	outPath := filepath.Join(scriptDir, *outputFile)

	// ── Phase 1: Find pods ──────────────────────────────────────────────────
	type appPod struct{ app, pod string }
	var (
		appPodsMu sync.Mutex
		appPods   []appPod
	)

	fmt.Println("Finding pods for apps...")
	doneCh1 := make(chan phaseItem, len(apps))
	var wg1 sync.WaitGroup
	for _, app := range apps {
		app := app
		wg1.Add(1)
		go func() {
			defer wg1.Done()
			pods, err := getPodsForApp(app, *namespace)
			if err != nil || len(pods) == 0 {
				doneCh1 <- phaseItem{label: app, result: "no pods found"}
				return
			}
			appPodsMu.Lock()
			for _, p := range pods {
				appPods = append(appPods, appPod{app, p})
			}
			appPodsMu.Unlock()
			doneCh1 <- phaseItem{label: app, result: fmt.Sprintf("%d pod(s)", len(pods))}
		}()
	}
	go func() { wg1.Wait(); close(doneCh1) }()
	phaseMonitor(len(apps), doneCh1)

	if len(appPods) == 0 {
		fmt.Fprintln(os.Stderr, "No pods found for the specified apps. Exiting.")
		os.Exit(1)
	}

	// ── Phase 2: Fetch containers ───────────────────────────────────────────
	type podContainers struct {
		app        string
		pod        string
		containers []string
	}
	var (
		podContainersMu  sync.Mutex
		allPodContainers []podContainers
	)

	fmt.Println("Fetching containers for pods...")
	doneCh2 := make(chan phaseItem, len(appPods))
	var wg2 sync.WaitGroup
	for _, ap := range appPods {
		ap := ap
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			containers, err := getContainersForPod(ap.pod, *namespace)
			if err != nil || len(containers) == 0 {
				doneCh2 <- phaseItem{label: ap.pod, result: "no containers"}
				return
			}
			podContainersMu.Lock()
			allPodContainers = append(allPodContainers, podContainers{ap.app, ap.pod, containers})
			podContainersMu.Unlock()
			doneCh2 <- phaseItem{label: ap.pod, result: fmt.Sprintf("%d container(s)", len(containers))}
		}()
	}
	go func() { wg2.Wait(); close(doneCh2) }()
	phaseMonitor(len(appPods), doneCh2)

	// ── Open output file ────────────────────────────────────────────────────
	outFile, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot open output file %s: %v\n", outPath, err)
		os.Exit(1)
	}
	defer outFile.Close()

	var fileMu sync.Mutex

	follow := *since == ""
	totalPods := len(appPods)
	if *since != "" {
		fmt.Printf("Showing historical logs from %s to now for %d pods and their containers.\n", *since, totalPods)
	} else {
		fmt.Printf("Starting log tailing for %d pods and their containers.\n", totalPods)
	}
	fmt.Printf("Logs are being saved to: %s\n", outPath)
	fmt.Println("Press Ctrl+C to stop all log streams")
	fmt.Println("----------------------------------------")

	// ── Set up signal handler for clean shutdown ────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Printf("\n%sCtrl+C received — stopping all log streams...%s\n", ansiYellow, ansiReset)
		outFile.Close()
		os.Exit(0)
	}()

	// ── Phase 3: Stream logs ────────────────────────────────────────────────
	var streams []*streamState
	var wg3 sync.WaitGroup

	// Show prep spinner while goroutines are being launched
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
			wg3.Add(1)
			go func(st *streamState) {
				defer wg3.Done()
				streamLogs(st, follow, *since, *namespace, pattern, outFile, &fileMu)
			}(st)
		}
	}

	// Run display monitor in foreground; wait for all streams after
	close(prepDone)
	displayMonitor(streams, *since)
	wg3.Wait()

	if *since != "" {
		fmt.Printf("\nHistorical logs collection completed. Logs saved to: %s\n", outPath)
	}
}
