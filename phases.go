package main

import (
	"fmt"
	"sync"

	"github.com/jedib0t/go-pretty/v6/progress"
)

// findPods resolves pods for each app in parallel and calls onDone for every
// completed item. Returns the flat (app, pod) list.
func findPods(apps []string, namespace string, onDone func(app string, pods []string, err error)) []appPod {
	var mu sync.Mutex
	var result []appPod
	doneCh := make(chan struct {
		app  string
		pods []string
		err  error
	}, len(apps))

	var wg sync.WaitGroup
	for _, app := range apps {
		app := app
		wg.Add(1)
		go func() {
			defer wg.Done()
			pods, err := getPodsForApp(app, namespace)
			doneCh <- struct {
				app  string
				pods []string
				err  error
			}{app, pods, err}
		}()
	}
	go func() { wg.Wait(); close(doneCh) }()

	for item := range doneCh {
		onDone(item.app, item.pods, item.err)
		if item.err == nil && len(item.pods) > 0 {
			mu.Lock()
			for _, p := range item.pods {
				result = append(result, appPod{item.app, p})
			}
			mu.Unlock()
		}
	}
	return result
}

// fetchContainers resolves containers for each pod in parallel and calls
// onDone for every completed item. Returns the flat pod→containers list.
func fetchContainers(pods []appPod, namespace string, onDone func(ap appPod, containers []string, err error)) []podContainers {
	var mu sync.Mutex
	var result []podContainers
	doneCh := make(chan struct {
		ap         appPod
		containers []string
		err        error
	}, len(pods))

	var wg sync.WaitGroup
	for _, ap := range pods {
		ap := ap
		wg.Add(1)
		go func() {
			defer wg.Done()
			containers, err := getContainersForPod(ap.pod, namespace)
			doneCh <- struct {
				ap         appPod
				containers []string
				err        error
			}{ap, containers, err}
		}()
	}
	go func() { wg.Wait(); close(doneCh) }()

	for item := range doneCh {
		onDone(item.ap, item.containers, item.err)
		if item.err == nil && len(item.containers) > 0 {
			mu.Lock()
			result = append(result, podContainers{item.ap.app, item.ap.pod, item.containers})
			mu.Unlock()
		}
	}
	return result
}

// runPhase1 finds all pods for the given apps in parallel, displaying a
// phaseMonitor progress bar. Returns the (app, pod) pairs.
func runPhase1(apps []string, namespace string) []appPod {
	monCh := make(chan phaseItem, len(apps))
	resultCh := make(chan []appPod, 1)
	go func() {
		resultCh <- findPods(apps, namespace, func(app string, pods []string, err error) {
			if err != nil || len(pods) == 0 {
				monCh <- phaseItem{label: app, result: "no pods found", ok: false}
			} else {
				monCh <- phaseItem{label: app, result: fmt.Sprintf("%d pod(s)", len(pods)), ok: true}
			}
		})
	}()
	phaseMonitor("Finding pods for apps...", apps, monCh)
	return <-resultCh
}

// runPhase2 fetches containers for every pod in parallel, displaying a
// phaseMonitor progress bar. Returns the full pod→containers mapping.
func runPhase2(pods []appPod, namespace string) []podContainers {
	podLabels := make([]string, len(pods))
	for i, ap := range pods {
		podLabels[i] = ap.pod
	}
	monCh := make(chan phaseItem, len(pods))
	resultCh := make(chan []podContainers, 1)
	go func() {
		resultCh <- fetchContainers(pods, namespace, func(ap appPod, containers []string, err error) {
			if err != nil || len(containers) == 0 {
				monCh <- phaseItem{label: ap.pod, result: "no containers", ok: false}
			} else {
				monCh <- phaseItem{label: ap.pod, result: fmt.Sprintf("%d container(s)", len(containers)), ok: true}
			}
		})
	}()
	phaseMonitor("Fetching containers for pods...", podLabels, monCh)
	return <-resultCh
}

// runPhase1Clean is like runPhase1 but updates an existing tracker instead of
// creating its own progress.Writer — used by clean mode.
func runPhase1Clean(apps []string, namespace string, tracker *progress.Tracker) []appPod {
	total := len(apps)
	done, errCount := 0, 0
	result := findPods(apps, namespace, func(app string, pods []string, err error) {
		done++
		tracker.Increment(1)
		tracker.UpdateMessage(cleanLabel(fmt.Sprintf("Finding pods... (%d/%d)", done, total)))
		if err != nil || len(pods) == 0 {
			errCount++
		}
	})
	if errCount > 0 {
		tracker.MarkAsErrored()
	} else {
		tracker.MarkAsDone()
	}
	return result
}

// runPhase2Clean is like runPhase2 but updates an existing tracker.
func runPhase2Clean(pods []appPod, namespace string, tracker *progress.Tracker) []podContainers {
	total := len(pods)
	tracker.UpdateTotal(int64(total))
	done, errCount := 0, 0
	result := fetchContainers(pods, namespace, func(ap appPod, containers []string, err error) {
		done++
		tracker.Increment(1)
		tracker.UpdateMessage(cleanLabel(fmt.Sprintf("Fetching containers... (%d/%d)", done, total)))
		if err != nil || len(containers) == 0 {
			errCount++
		}
	})
	if errCount > 0 {
		tracker.MarkAsErrored()
	} else {
		tracker.MarkAsDone()
	}
	return result
}
