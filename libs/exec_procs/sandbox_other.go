//go:build !linux && !darwin && !windows

package exec_procs

// probeBackend（其他平台）：无可用后端，fail-closed。
func probeBackend() sandboxBackend {
	return backendUnavailable
}

// planConfined（其他平台）：fail-closed——confined 模式拒绝执行。
func planConfined(level int, workdir string, argv []string) (launchPlan, error) {
	return launchPlan{}, sandboxUnavailable(level)
}

// cacheRoots（其他平台）：无。
func cacheRoots() []string { return nil }
