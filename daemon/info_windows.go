package daemon

import (
	"context"

	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/pkg/sysinfo"
	"github.com/moby/moby/api/types"
	"github.com/moby/moby/api/types/system"
)

// fillPlatformInfo fills the platform related info.
func (daemon *Daemon) fillPlatformInfo(ctx context.Context, v *system.Info, sysInfo *sysinfo.SysInfo, cfg *configStore) error {
	if _, ok := cfg.Features["windows-dns-proxy"]; ok {
		v.Warnings = append(v.Warnings, `
WARNING: Feature flag "windows-dns-proxy" has been removed, forwarding to external DNS resolvers is enabled.`)
	}
	return nil
}

func (daemon *Daemon) fillPlatformVersion(ctx context.Context, v *types.Version, cfg *configStore) error {
	return nil
}

func fillDriverWarnings(v *system.Info) {
}

func cgroupNamespacesEnabled(sysInfo *sysinfo.SysInfo, cfg *config.Config) bool {
	return false
}

// Rootless returns true if daemon is running in rootless mode
func Rootless(*config.Config) bool {
	return false
}

func noNewPrivileges(*config.Config) bool {
	return false
}
