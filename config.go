package main

import "time"

// ─── Application defaults ─────────────────────────────────────────────────────

const (
	// defaultOutputFile is the log file written when -o is not provided.
	defaultOutputFile = "tail_multiple_logs_data.log"

	// defaultStreamTimeout caps each kubectl stream in collect (-s) mode.
	defaultStreamTimeout = 2 * time.Minute

	// defaultErrorTerms is the pipe-separated filter applied by -e.
	defaultErrorTerms = "ERROR|WARN|Exception|failed|error"
)

// ─── Progress display ─────────────────────────────────────────────────────────

const (
	// progressUpdateFreq controls how often the progress bars are redrawn.
	progressUpdateFreq = 80 * time.Millisecond

	// progressLabelWidth is the fixed column width for clean-mode bar labels.
	// Derived from the widest possible label: "Fetching containers... (999/999)"
	// = 32 chars, padded to 36 for a comfortable margin.
	progressLabelWidth = 36
)

// ─── Summary table ────────────────────────────────────────────────────────────

const (
	// summaryTimeLayout is the timestamp format used in the summary table.
	summaryTimeLayout = "15:04:05"

	// truncErrMsgLen is the max chars for an error message shown in the
	// verbose progress monitor (per-stream row).
	truncErrMsgLen = 60

	// truncPodNameLen is the max chars for a pod name shown in the verbose
	// progress monitor's slowest-stream hint.
	truncPodNameLen = 28

	// truncSummaryErrLen is the max chars for an error message shown in the
	// summary table.
	truncSummaryErrLen = 40
)
