package node

import (
	"context"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"

	authpb "github.com/tareksalem/falak/node/proto/authpb"
)

// sampleHostCapabilities reads CPU cores + total memory + total disk
// (root partition) from gopsutil and returns them as an authpb.Capabilities
// the Authenticator stamps onto the outbound JoinRequest. The datacenter
// the operator declared at startup is preserved.
//
// All calls are bounded to a short context so a hung kernel call cannot
// stall node startup. Failures fall back to zero values for that field
// rather than aborting — the cluster still works without capability
// reporting, the CLI display just shows 0.
func sampleHostCapabilities(datacenter string) *authpb.Capabilities {
	caps := &authpb.Capabilities{Datacenter: datacenter}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if cores, err := cpu.CountsWithContext(ctx, true); err == nil {
		caps.CpuCores = int32(cores)
	}
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		caps.MemoryMb = int64(vm.Total / (1024 * 1024))
	}
	if du, err := disk.UsageWithContext(ctx, "/"); err == nil {
		caps.DiskGb = int64(du.Total / (1024 * 1024 * 1024))
	}
	return caps
}
