package main

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jedib0t/go-pretty/v6/progress"
	"github.com/jedib0t/go-pretty/v6/text"
)

// activeStop holds a func() that stops whichever progress.Writer is currently
// rendering, so the signal handler can halt it before printing the Ctrl+C banner.
var (
	activePWMu  sync.Mutex
	activeStop  func()
)

// clearLine erases the current terminal line and moves cursor to column 0.
func clearLine() { fmt.Print("\r\033[K") }

// printSpinner overwrites the current line with a spinner (no newline).
func printSpinner(spin, msg string) {
	fmt.Printf("\r\033[K  %s  %s", text.FgYellow.Sprint(spin), msg)
}

// ─── Phase monitor (finding pods / fetching containers) ──────────────────────

// phaseItem carries the result of one parallel task once it finishes.
type phaseItem struct {
	label  string
	result string
	ok     bool
}

// newPW returns a configured progress.Writer ready to use.
func newPW() progress.Writer {
	pw := progress.NewWriter()
	pw.SetAutoStop(false)
	pw.SetStyle(progress.StyleBlocks)
	pw.Style().Colors = progress.StyleColors{
		Message: text.Colors{},
		Error:   text.Colors{text.FgRed},
		Stats:   text.Colors{text.FgHiBlack},
		Time:    text.Colors{text.FgHiBlack},
		Tracker: text.Colors{text.FgGreen},
		Value:   text.Colors{text.FgHiBlack},
		Speed:   text.Colors{text.FgHiBlack},
	}
	pw.Style().Chars.Finished = "█"
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
	pw.SetUpdateFrequency(progressUpdateFreq)
	return pw
}

// phaseMonitor shows a live progress bar for a batch of parallel tasks.
// One real progress bar tracks overall completions; individual item trackers
// are appended and immediately marked done as each result arrives (one-by-one).
func phaseMonitor(title string, labels []string, doneCh <-chan phaseItem) {
	total := len(labels)
	pw := newPW()

	overall := &progress.Tracker{
		Message: fmt.Sprintf("  %s  0 / %d", text.Bold.Sprint(title), total),
		Total:   int64(total),
	}
	pw.AppendTracker(overall)
	go pw.Render()

	for i := range labels {
		item := <-doneCh
		resultStr := text.FgHiBlack.Sprint(item.result)
		if !item.ok {
			resultStr = text.FgRed.Sprint(item.result)
		}
		// Append individual item — Total:0 means no bar is shown for it.
		t := &progress.Tracker{
			Message: fmt.Sprintf("  %-52s  %s", item.label, resultStr),
			Total:   0,
		}
		pw.AppendTracker(t)
		if item.ok {
			t.MarkAsDone()
		} else {
			t.MarkAsErrored()
		}
		overall.Increment(1)
		if n := i + 1; n < total {
			overall.UpdateMessage(fmt.Sprintf("  %s  %d / %d", text.Bold.Sprint(title), n, total))
		}
	}
	overall.MarkAsDone()
	pw.Stop()
}

// ─── Display monitor (Phase 3) ────────────────────────────────────────────────

// displayMonitor tracks every log stream with a go-pretty progress display.
// One real progress bar shows overall completion; streams are appended and
// immediately marked done one-by-one as they finish.
func displayMonitor(streams []*streamState, since string) {
	// Sort by app then pod for consistent grouping.
	sorted := make([]*streamState, len(streams))
	copy(sorted, streams)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].app != sorted[j].app {
			return sorted[i].app < sorted[j].app
		}
		return sorted[i].pod < sorted[j].pod
	})

	total := len(sorted)
	verb := "collected"
	verbCap := "Collecting"
	if since == "" {
		verb = "following"
		verbCap = "Following"
	}

	pw := newPW()

	overall := &progress.Tracker{
		Message: fmt.Sprintf("  %s logs… (0 / %d done)", verbCap, total),
		Total:   int64(total),
	}
	pw.AppendTracker(overall)

	// Register a stop function for signal-handler cleanup.
	activePWMu.Lock()
	activeStop = pw.Stop
	activePWMu.Unlock()
	defer func() {
		activePWMu.Lock()
		activeStop = nil
		activePWMu.Unlock()
	}()

	go pw.Render()

	printedApp := map[string]bool{}
	printedStream := make([]bool, total)
	doneCount := 0
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	for doneCount < total {
		<-ticker.C

		// Flush newly-finished streams as permanent trackers.
		for i, st := range sorted {
			if printedStream[i] || !st.isDone() {
				continue
			}
			// App section header — printed once per app.
			if !printedApp[st.app] {
				sep := &progress.Tracker{
					Message: "  " + text.Bold.Sprint(st.app),
					Total:   0,
				}
				pw.AppendTracker(sep)
				sep.MarkAsDone()
				printedApp[st.app] = true
			}
			// Stream result tracker.
			var msg string
			switch {
			case st.isFailed():
				msg = fmt.Sprintf("    [%s] %s  %s",
					text.FgCyan.Sprint(st.pod), st.container,
					text.FgRed.Sprint(truncate(st.errMsg, truncErrMsgLen)))
			case st.isTimedOut():
				msg = fmt.Sprintf("    [%s] %s  %s",
					text.FgCyan.Sprint(st.pod), st.container,
					text.FgYellow.Sprintf("timed out · %d lines", st.lineCount()))
			default:
				msg = fmt.Sprintf("    [%s] %s  %s",
					text.FgCyan.Sprint(st.pod), st.container,
					text.FgHiBlack.Sprintf("%s · %d lines", verb, st.lineCount()))
			}
			t := &progress.Tracker{Message: msg, Total: 0}
			pw.AppendTracker(t)
			if st.isFailed() {
				t.MarkAsErrored()
			} else {
				t.MarkAsDone()
			}
			printedStream[i] = true
			doneCount++
			overall.Increment(1)
		}

		// Keep the overall tracker message fresh with the slowest pending stream.
		if doneCount < total {
			var slowest *streamState
			for _, st := range sorted {
				if !st.isDone() && (slowest == nil || st.launchedAt.Before(slowest.launchedAt)) {
					slowest = st
				}
			}
			msg := fmt.Sprintf("  %s logs… (%d / %d done)", verbCap, doneCount, total)
			if slowest != nil {
				msg += "  ·  " + text.FgHiBlack.Sprintf("[%s] %s",
					truncate(slowest.pod, truncPodNameLen), slowest.statusLabel())
			}
			overall.UpdateMessage(msg)
		}
	}
	pw.Stop()
}

// ─── Clean-mode progress helpers ─────────────────────────────────────────────

// cleanLabel returns a bold, fixed-width label for clean-mode progress bars
// so all three bars start at the same horizontal column.
// labelWidth is derived from the widest string any of the three phases can
// produce: "Fetching containers... (999/999)" = 32 chars → padded to 36.
func cleanLabel(s string) string {
	return fmt.Sprintf("  %s", text.Bold.Sprint(fmt.Sprintf("%-*s", progressLabelWidth, s)))
}

// displayMonitorClean is like displayMonitor but updates an existing tracker
// without appending per-stream rows — default mode (use -verbose for detail).
func displayMonitorClean(streams []*streamState, verbCap string, tracker *progress.Tracker) {
	total := len(streams)
	tracker.UpdateTotal(int64(total))

	printed := make([]bool, total)
	doneCount := 0
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for doneCount < total {
		<-ticker.C
		for i, st := range streams {
			if printed[i] || !st.isDone() {
				continue
			}
			tracker.Increment(1)
			printed[i] = true
			doneCount++
			tracker.UpdateMessage(cleanLabel(fmt.Sprintf("%s logs... (%d/%d)", verbCap, doneCount, total)))
		}
	}
	tracker.MarkAsDone()
}
