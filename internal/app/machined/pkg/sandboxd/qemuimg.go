// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package sandboxd

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/containerd/cgroups/v3/cgroup2"

	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/internal/pkg/cgroup"
	"github.com/siderolabs/talos/internal/pkg/qemuimg"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// qemuImgReadOnlyPaths is everything qemu-img sees of the host: the binary, its
// shared libraries and the musl loader configuration. Images arrive as
// descriptors, so no pool, library or socket path is visible.
var qemuImgReadOnlyPaths = []string{
	qemuimg.Binary,
	"/usr/local/lib",
	"/usr/lib",
	"/lib",
	"/etc/ld-musl-x86_64.path",
	"/etc/ld-musl-aarch64.path",
	"/dev/urandom",
}

// QemuImgRunner runs qemu-img isolated in the sandbox, in its own cgroup with
// hard memory, CPU and PID limits. A run killed for memory is a permanent
// image failure, never retried.
func QemuImgRunner(r runtime.Runtime) qemuimg.Runner {
	period := uint64(100_000)
	quota := int64(constants.CgroupQemuImgMillicores) * int64(period) / 1000

	resources := &cgroup2.Resources{
		Memory: &cgroup2.Memory{
			Max:  new(int64(constants.CgroupQemuImgMaxMemory)),
			Swap: new(int64(0)),
		},
		CPU: &cgroup2.CPU{
			Max:    cgroup2.NewCPUMax(&quota, &period),
			Weight: new(cgroup.MillicoresToCPUWeight(cgroup.MilliCores(constants.CgroupQemuImgMillicores))),
		},
		Pids: &cgroup2.Pids{Max: constants.CgroupQemuImgMaxPids},
	}

	return func(ctx context.Context, binary string, args []string, files []*os.File, stdout, stderr io.Writer) (int, error) {
		code, err := RunIsolated(ctx, r.Sandbox(), OneShot{
			Args:          append([]string{binary}, args...),
			Files:         files,
			ReadOnlyPaths: qemuImgReadOnlyPaths,
			SelinuxLabel:  constants.SelinuxLabelQemuImg,
			Cgroup:        constants.CgroupQemuImg,
			Resources:     resources,
			Stdout:        stdout,
			Stderr:        stderr,
		})
		if errors.Is(err, ErrMemoryLimit) {
			return code, &qemuimg.ValidationError{Err: err}
		}

		return code, err
	}
}
