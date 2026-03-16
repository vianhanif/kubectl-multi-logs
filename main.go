package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jedib0t/go-pretty/v6/progress"
	"github.com/jedib0t/go-pretty/v6/text"
)

const defaultOutput = "tail_multiple_logs_data.log"

var braille = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// truncate shortens s to at most n bytes, appending "..." if cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}








// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	// Pre-process bare "-o" (no following value) before flag.Parse so we only
	// parse once. The flag package would otherwise error on the next token.
	for i, arg := range os.Args[1:] {
		if arg == "-o" {
			os.Args[i+1] = "-o=" + defaultOutput
			break
		}
	}

	var (
		namespace     = flag.String("n", "", "Kubernetes namespace")
		since         = flag.String("s", "", "Show logs since (e.g. 10m, 1h)")
		grepPattern   = flag.String("g", "", "Filter log lines (case-insensitive, supports | for multiple patterns)")
		errorsOnly    = flag.Bool("e", false, "Filter for ERROR/WARN/Exception/failed/error")
		outputFile    = flag.String("o", defaultOutput, "Output file name (-o alone uses the default)")
		streamTimeout = flag.Duration("T", 2*time.Minute, "Per-stream timeout in collect mode (-s); 0 = no limit")
		verbose       = flag.Bool("verbose", false, "Show per-item detail during progress (pod names, containers, stream results)")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [-n namespace] [-s since] [-T timeout] [-g pattern] [-e] [-o [output_file]] <app1> <app2> ...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	apps := flag.Args()
	if len(apps) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	pattern := buildPattern(*grepPattern, *errorsOnly)

	scriptDir, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	outPath := filepath.Join(scriptDir, *outputFile)

	verbCap := "Collecting"
	if *since == "" {
		verbCap = "Following"
	}

	// ── Phase 1 & 2 ─────────────────────────────────────────────────────────
	var appPods []appPod
	var allPodContainers []podContainers
	var cleanPW progress.Writer
	var cleanRenderWg sync.WaitGroup
	var cleanT3 *progress.Tracker

	if !*verbose {
		// Print one-line header before the bars appear.
		if *since != "" {
			fmt.Printf("%s  ·  saving to %s\n",
				text.Bold.Sprintf("Collecting from last %s", *since),
				text.FgHiBlack.Sprint(outPath))
		} else {
			fmt.Printf("%s  ·  saving to %s\n",
				text.Bold.Sprint("Following live logs"),
				text.FgHiBlack.Sprint(outPath))
		}

		pw := newPW()
		t1 := &progress.Tracker{Message: cleanLabel("Finding pods..."), Total: int64(len(apps))}
		t2 := &progress.Tracker{Message: cleanLabel("Fetching containers..."), Total: 0}
		t3 := &progress.Tracker{Message: cleanLabel(verbCap + " logs..."), Total: 0}
		pw.AppendTracker(t1)
		pw.AppendTracker(t2)
		pw.AppendTracker(t3)
		activePWMu.Lock()
		activeStop = pw.Stop
		activePWMu.Unlock()
		cleanRenderWg.Add(1)
		go func() { defer cleanRenderWg.Done(); pw.Render() }()
		cleanPW = pw
		cleanT3 = t3

		appPods = runPhase1Clean(apps, *namespace, t1)
		if len(appPods) == 0 {
			pw.Stop()
			fmt.Fprintln(os.Stderr, "No pods found for the specified apps. Exiting.")
			os.Exit(1)
		}
		allPodContainers = runPhase2Clean(appPods, *namespace, t2)

		// Update t3 label with the total stream count now that it's known.
		totalStreams := 0
		for _, pc := range allPodContainers {
			totalStreams += len(pc.containers)
		}
		t3.UpdateMessage(cleanLabel(fmt.Sprintf("%s logs...  (%d streams)", verbCap, totalStreams)))
	} else {
		appPods = runPhase1(apps, *namespace)
		if len(appPods) == 0 {
			fmt.Fprintln(os.Stderr, "No pods found for the specified apps. Exiting.")
			os.Exit(1)
		}
		allPodContainers = runPhase2(appPods, *namespace)
	}

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

	if *verbose {
		if cfg.since != "" {
			fmt.Printf("Showing historical logs from %s to now for %d pods and their containers.\n", cfg.since, len(appPods))
		} else {
			fmt.Printf("Starting log tailing for %d pods and their containers.\n", len(appPods))
		}
		fmt.Printf("Logs are being saved to: %s\n", outPath)
		fmt.Println("Press Ctrl+C to stop all log streams")
		fmt.Println("----------------------------------------")
	}

	// ── Signal handler ──────────────────────────────────────────────────────
	var streamsStore atomic.Value
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		// Stop any live progress writer so its render loop doesn't race
		// with the Ctrl+C banner we're about to print.
		activePWMu.Lock()
		stop := activeStop
		activePWMu.Unlock()
		if stop != nil {
			stop()
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

	start := time.Now()
	if !*verbose {
		displayMonitorClean(streams, verbCap, cleanT3)
		cleanPW.Stop()
		cleanRenderWg.Wait()
		activePWMu.Lock()
		activeStop = nil
		activePWMu.Unlock()
	} else {
		displayMonitor(streams, cfg.since)
	}
	wg.Wait()
	elapsed := time.Since(start).Round(time.Second)

	// All N kubectl processes have exited; no hidden work remains.
	// If one stream ran much longer than the others it is simply the slowest
	// pod — the -T flag (default 2m) caps any runaway stream.
	fmt.Printf("%s  %s\n",
		text.FgGreen.Sprint("✔ All streams finished"),
		text.FgHiBlack.Sprintf("total elapsed: %s", elapsed),
	)

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


