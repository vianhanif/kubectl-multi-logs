package main

import (
	"fmt"
	"sync"

	"github.com/jedib0t/go-pretty/v6/progress"
)

// runPhase1 finds all pods for the given apps in parallel and returns
// the (app, pod) pairs. The progress is displayed via phaseMonitor.
func runPhase1(apps []string, namespace string) []appPod {
	var mu sync.Mutex
	var result []appPod

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
	phaseMonitor("Finding pods for apps...", apps, doneCh)
	return result
}

// runPhase2 fetches containers for every pod in parallel and returns
// the full pod→containers mapping. The progress is displayed via phaseMonitor.
func runPhase2(pods []appPod, namespace string) []podContainers {
	var mu sync.Mutex
	var result []podContainers

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
	phaseMonitor("Fetching containers for pods...", podLabels, doneCh)
	return result
}

// runPhase1Clean is like runPhase1 but updates an existing tracker instead of
// creating its own progress.Writer — used by clean mode.
func runPhase1Clean(apps []string, namespace string, tracker *progress.Tracker) []appPod {
	var mu sync.Mutex
	var result []appPod
	doneCh := make(chan phaseItem, len(apps))
	var wg sync.WaitGroup
	for _, app := range apps {
		app := app
		wg.Add(1)
		go func() {
			defer wg.Done()
			pods, err := getPodsForApp(app, namespace)
			if err != nil || len(pods) == 0 {
				doneCh <- phaseItem{label: app, ok: false}
				return
			}
			mu.Lock()
			for _, p := range pods {
				result = append(result, appPod{app, p})
			}
			mu.Unlock()
			doneCh <- phaseItem{label: app, ok: true}
		}()
	}
	go func() { wg.Wait(); close(doneCh) }()
	total := len(apps)
	errCount, done := 0, 0
	for range apps {
		item := <-doneCh
		done++
		tracker.Increment(1)
		tracker.UpdateMessage(cleanLabel(fmt.Sprintf("Finding pods... (%d/%d)", done, total)))
		if !item.ok {
			errCount++
		}
	}
	if errCount > 0 {
		tracker.MarkAsErrored()
	} else {
		tracker.MarkAsDone()
	}
	return result
}

// runPhase2Clean is like runPhase2 but updates an existing tracker.
func runPhase2Clean(pods []appPod, namespace string, tracker *progress.Tracker) []podContainers {
	var mu sync.Mutex
	var result []podContainers
	total := len(pods)
	tracker.UpdateTotal(int64(total))
	doneCh := make(chan phaseItem, total)
	var wg sync.WaitGroup
	for _, ap := range pods {
		ap := ap
		wg.Add(1)
		go func() {
			defer wg.Done()
			containers, err := getContainersForPod(ap.pod, namespace)
			if err != nil || len(containers) == 0 {
				doneCh <- phaseItem{label: ap.pod, ok: false}
				return
			}
			mu.Lock()
			result = append(result, podContainers{ap.app, ap.pod, containers})
			mu.Unlock()
			doneCh <- phaseItem{label: ap.pod, ok: true}
		}()
	}
	go func() { wg.Wait(); close(doneCh) }()
	errCount, done := 0, 0
	for range pods {
		item := <-doneCh
		done++
		tracker.Increment(1)
		tracker.UpdateMessage(cleanLabel(fmt.Sprintf("Fetching containers... (%d/%d)", done, total)))
		if !item.ok {
			errCount++
		}
	}
	if errCount > 0 {
		tracker.MarkAsErrored()
	} else {
		tracker.MarkAsDone()
	}
	return result
}
