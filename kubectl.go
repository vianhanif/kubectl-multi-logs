package main

import (
	"os/exec"
	"strings"
)

// ─── Domain types ─────────────────────────────────────────────────────────────

// appPod associates a pod name with its owning app label.
type appPod struct{ app, pod string }

// podContainers holds the container list for a single pod.
type podContainers struct {
	app        string
	pod        string
	containers []string
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
