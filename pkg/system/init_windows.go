package system

// containerdRuntimeSupported determines if containerd should be the runtime.
var containerdRuntimeSupported = false

// EnableContainerdRuntime sets whether to use containerd for runtime on Windows.
func EnableContainerdRuntime(cdPath string) {
	if len(cdPath) > 0 {
		containerdRuntimeSupported = true
	}
}

// ContainerdRuntimeSupported returns true if the use of containerd runtime is supported.
func ContainerdRuntimeSupported() bool {
	return containerdRuntimeSupported
}
