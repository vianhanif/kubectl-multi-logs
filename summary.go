package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

// printSummary prints a table summary grouped by app.
// The app name appears as a bold header row in the Pod column; each container
// stream follows as a data row with pod, container, status, lines, and time range.
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
	fmt.Println()

	tw := table.NewWriter()
	tw.SetOutputMirror(os.Stdout)
	tw.SetStyle(table.StyleRounded)
	tw.Style().Options.SeparateRows = false
	tw.AppendHeader(table.Row{"Pod", "Container", "", "Lines", "Time range"})

	for i, app := range appOrder {
		g := groups[app]

		// App header row — spans the Pod column, rest empty.
		appMeta := text.FgHiBlack.Sprintf("%d pod(s) · %d stream(s) · %d lines", len(g.pods), len(g.containers), g.lines)
		if g.errors > 0 {
			appMeta += "  " + text.FgRed.Sprintf("%d err", g.errors)
		}
		if g.timedOuts > 0 {
			appMeta += "  " + text.FgYellow.Sprintf("%d timed out", g.timedOuts)
		}
		tw.AppendRow(table.Row{
			text.Bold.Sprint(app) + "  " + appMeta, "", "", "", "",
		})

		// Group containers by pod, preserving sorted order.
		var podOrder []string
		podMap := map[string][]*streamState{}
		for _, st := range g.containers {
			if _, ok := podMap[st.pod]; !ok {
				podOrder = append(podOrder, st.pod)
			}
			podMap[st.pod] = append(podMap[st.pod], st)
		}

		for _, pod := range podOrder {
			for rowIdx, st := range podMap[pod] {
				// Pod name only on the first container row for that pod.
				podCell := ""
				if rowIdx == 0 {
					podCell = text.FgHiBlack.Sprint(pod)
				}

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
					timeRange = text.FgHiBlack.Sprintf("%s → %s",
						st.startedAt.Format(tsLayout), st.lastAt.Format(tsLayout))
					if st.isTimedOut() {
						timeRange += text.FgYellow.Sprint(" (cut)")
					}
				} else if st.isFailed() {
					timeRange = text.FgRed.Sprint(truncate(st.errMsg, 40))
				}

				tw.AppendRow(table.Row{
					podCell,
					text.FgCyan.Sprint(st.container),
					icon,
					text.Bold.Sprintf("%d", st.lineCount()),
					timeRange,
				})
			}
		}

		if i < len(appOrder)-1 {
			tw.AppendSeparator()
		}
	}

	tw.AppendFooter(table.Row{
		text.Bold.Sprint("TOTAL"),
		text.FgHiBlack.Sprintf("%d app(s) · %d pod(s) · %d stream(s)", len(appOrder), totalPods, totalContainers),
		"", text.Bold.Sprintf("%d", totalLines), "",
	})
	tw.Render()
	fmt.Println()
}
