// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package sandboxd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/containerd/cgroups/v3/cgroup2"

	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/internal/pkg/cgroup"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// ErrUnavailable reports that the sandbox namespace is not running yet, or is
// being recreated. A later attempt can succeed.
var ErrUnavailable = errors.New("sandbox namespace not available")

// ErrMemoryLimit reports that the kernel killed an isolated run for exceeding
// its cgroup memory limit.
var ErrMemoryLimit = errors.New("memory limit exceeded")

// OneShot is a single isolated run of a tool to completion.
type OneShot struct {
	Args []string
	Env  []string
	// Files become descriptors 3, 4, ... of the tool.
	Files []*os.File
	// ReadOnlyPaths are the only host paths the tool can see; they must not nest.
	ReadOnlyPaths []string
	SelinuxLabel  string
	// Cgroup is the parent of the transient cgroup created for this run.
	Cgroup    string
	Resources *cgroup2.Resources
	Stdout    io.Writer
	Stderr    io.Writer
}

// RunIsolated runs spec in fresh namespaces and its own cgroup, and returns the
// tool's exit code. Canceling ctx kills the tool. A signaled tool returns an
// error; one killed for memory wraps ErrMemoryLimit.
//
//nolint:gocyclo
func RunIsolated(ctx context.Context, launcher runtime.SandboxLauncher, spec OneShot) (int, error) {
	if launcher == nil {
		return -1, ErrUnavailable
	}

	run, err := newRunCgroup(spec.Cgroup, spec.Resources)
	if err != nil {
		return -1, err
	}

	defer run.close() //nolint:errcheck

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return -1, err
	}

	defer stdoutR.Close() //nolint:errcheck

	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutW.Close() //nolint:errcheck

		return -1, err
	}

	defer stderrR.Close() //nolint:errcheck

	handle, err := launcher.Launch(runtime.LaunchConfig{
		Args:         spec.Args,
		Env:          spec.Env,
		SelinuxLabel: spec.SelinuxLabel,
		Stdout:       stdoutW,
		Stderr:       stderrW,
		ExtraFiles:   spec.Files,
		Isolation: &runtime.Isolation{
			Cgroup:        run.dir,
			ReadOnlyPaths: spec.ReadOnlyPaths,
		},
	})

	// The tool holds its own copies; ours would keep the readers from seeing EOF.
	stdoutW.Close() //nolint:errcheck
	stderrW.Close() //nolint:errcheck

	if err != nil {
		return -1, errors.Join(ErrUnavailable, err)
	}

	defer handle.Close() //nolint:errcheck

	var copies sync.WaitGroup

	copies.Go(func() { io.Copy(writerOrDiscard(spec.Stdout), stdoutR) }) //nolint:errcheck
	copies.Go(func() { io.Copy(writerOrDiscard(spec.Stderr), stderrR) }) //nolint:errcheck

	type result struct {
		code int
		err  error
	}

	done := make(chan result, 1)

	go func() {
		code, waitErr := handle.Wait()
		done <- result{code, waitErr}
	}()

	var exit result

	select {
	case exit = <-done:
	case <-ctx.Done():
		// The tool is PID 1 of its namespace and ignores SIGTERM.
		handle.Signal(syscall.SIGKILL) //nolint:errcheck

		exit = <-done
	}

	copies.Wait()

	switch {
	case ctx.Err() != nil:
		return -1, ctx.Err()
	case run.oomKilled():
		return -1, fmt.Errorf("%s: %w", spec.Args[0], ErrMemoryLimit)
	case exit.err != nil && exit.code < 0:
		return -1, fmt.Errorf("%s: %w", spec.Args[0], exit.err)
	}

	return exit.code, nil
}

func writerOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}

	return w
}

// runCgroup is the transient cgroup of one isolated run.
type runCgroup struct {
	manager *cgroup2.Manager
	dir     *os.File
}

func newRunCgroup(parent string, resources *cgroup2.Resources) (*runCgroup, error) {
	if resources == nil {
		resources = &cgroup2.Resources{}
	}

	var suffix [8]byte

	rand.Read(suffix[:]) //nolint:errcheck

	name := cgroup.Path(filepath.Join(parent, hex.EncodeToString(suffix[:])))

	manager, err := cgroup2.NewManager(constants.CgroupMountPath, name, resources)
	if err != nil {
		return nil, fmt.Errorf("create cgroup %s: %w", name, err)
	}

	dir, err := os.Open(filepath.Join(constants.CgroupMountPath, name))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open cgroup %s: %w", name, err), manager.Delete())
	}

	return &runCgroup{manager: manager, dir: dir}, nil
}

// oomKilled reports whether the kernel killed a process for exceeding memory.max.
func (c *runCgroup) oomKilled() bool {
	data, err := os.ReadFile(filepath.Join(c.dir.Name(), "memory.events"))
	if err != nil {
		return false
	}

	for line := range strings.Lines(string(data)) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "oom_kill "); ok {
			count, parseErr := strconv.ParseUint(value, 10, 64)

			return parseErr == nil && count > 0
		}
	}

	return false
}

// close kills anything left behind and removes the cgroup.
func (c *runCgroup) close() error {
	return errors.Join(c.dir.Close(), c.manager.Kill(), c.manager.Delete())
}
