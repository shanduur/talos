// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package sandboxd

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// sandboxdPath is the machined binary under its sandboxd personality.
const sandboxdPath = "/sbin/sandboxd"

// isolateCommand selects the confinement helper personality of sandboxd.
const isolateCommand = "isolate"

// isolatedCloneFlags gives an isolated launch nothing to share with the
// container plane or the host: no processes to signal, no network to reach.
const isolatedCloneFlags = syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWNET |
	syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS | syscall.CLONE_NEWCGROUP

// isolateRoot is overmounted with an empty tmpfs inside the new mount namespace
// and becomes the root of the confined process.
const isolateRoot = "/run"

// isolateArgs encodes the helper command line. sandboxd re-executes its own
// binary (/proc/self/exe); argv[0] selects the sandboxd personality:
//
//	sandboxd isolate <extra files> <read-only path>... -- <args>...
func isolateArgs(req launchRequest) []string {
	args := append([]string{sandboxdPath, isolateCommand, strconv.Itoa(req.ExtraFiles)}, req.ReadOnlyPaths...)

	return append(append(args, "--"), req.Args...)
}

func parseIsolateArgs(args []string) (files int, readOnly, command []string, err error) {
	separator := slices.Index(args, "--")
	if separator < 1 || separator == len(args)-1 {
		return 0, nil, nil, errors.New("usage: sandboxd isolate FILES [PATH]... -- COMMAND [ARG]")
	}

	files, err = strconv.Atoi(args[0])
	if err != nil || files < 0 {
		return 0, nil, nil, fmt.Errorf("invalid file count %q", args[0])
	}

	readOnly = args[1:separator]

	for _, path := range readOnly {
		if !validReadOnlyPath(path) {
			return 0, nil, nil, fmt.Errorf("invalid read-only path %q", path)
		}
	}

	return files, readOnly, args[separator+1:], nil
}

// validReadOnlyPath accepts clean absolute paths outside isolateRoot, whose
// tmpfs would hide them.
func validReadOnlyPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" &&
		path != isolateRoot && !strings.HasPrefix(path, isolateRoot+"/")
}

// isolateMain runs as PID 1 of fresh namespaces, builds the confined root and
// replaces itself with the requested command. It never returns on success.
func isolateMain(args []string) {
	if err := isolate(args); err != nil {
		fmt.Fprintf(os.Stderr, "sandboxd isolate: %v\n", err) //nolint:errcheck

		os.Exit(126)
	}
}

func isolate(args []string) error {
	files, readOnly, command, err := parseIsolateArgs(args)
	if err != nil {
		return err
	}

	// Credentials and no_new_privs are per thread; execve uses this thread's.
	runtime.LockOSThread()

	if err := buildRoot(readOnly); err != nil {
		return err
	}

	// Only stdio and the passed files survive the exec.
	if err := unix.CloseRange(uint(3+files), math.MaxUint32, unix.CLOSE_RANGE_CLOEXEC); err != nil {
		return fmt.Errorf("close_range: %w", err)
	}

	if err := dropPrivileges(); err != nil {
		return err
	}

	return syscall.Exec(command[0], command, os.Environ())
}

// buildRoot pivots into an empty read-only tmpfs holding only the allowed
// paths, /dev/null and a /proc scoped to the new PID namespace.
//
//nolint:gocyclo
func buildRoot(readOnly []string) error {
	// Nothing below propagates back into the sandbox or host namespaces.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}

	if err := unix.Mount("tmpfs", isolateRoot, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755,size=1m"); err != nil {
		return fmt.Errorf("mount root tmpfs: %w", err)
	}

	for _, path := range readOnly {
		if err := bindReadOnly(path, filepath.Join(isolateRoot, path)); err != nil {
			return err
		}
	}

	if err := bindReadOnly("/dev/null", filepath.Join(isolateRoot, "dev/null")); err != nil {
		return err
	}

	proc := filepath.Join(isolateRoot, "proc")

	if err := os.Mkdir(proc, 0o555); err != nil {
		return err
	}

	if err := unix.Mount("proc", proc, "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}

	old := filepath.Join(isolateRoot, ".old")

	if err := os.Mkdir(old, 0o700); err != nil {
		return err
	}

	if err := unix.PivotRoot(isolateRoot, old); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}

	if err := unix.Chdir("/"); err != nil {
		return err
	}

	if err := unix.Unmount("/.old", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}

	if err := os.Remove("/.old"); err != nil {
		return err
	}

	if err := unix.Mount("", "/", "", unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		return fmt.Errorf("remount root read-only: %w", err)
	}

	return nil
}

// bindReadOnly mirrors source at target. A missing source is skipped, so
// optional files such as loader configuration need not exist.
func bindReadOnly(source, target string) error {
	info, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	if info.IsDir() {
		err = os.MkdirAll(target, 0o755)
	} else {
		err = os.MkdirAll(filepath.Dir(target), 0o755)
		if err == nil {
			err = os.WriteFile(target, nil, 0o444)
		}
	}

	if err != nil {
		return err
	}

	if err := unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind %s: %w", source, err)
	}

	flags := uintptr(unix.MS_REMOUNT | unix.MS_BIND | unix.MS_RDONLY | unix.MS_NOSUID)
	if !strings.HasPrefix(source, "/dev/") {
		flags |= unix.MS_NODEV
	}

	if err := unix.Mount("", target, "", flags, ""); err != nil {
		return fmt.Errorf("remount %s read-only: %w", source, err)
	}

	return nil
}

// dropPrivileges leaves uid 0 without any capability, now or after execve:
// an empty bounding set keeps the kernel from granting root its usual set.
func dropPrivileges() error {
	all := make([]cap.Value, 0, cap.MaxBits())
	for value := range cap.MaxBits() {
		all = append(all, value)
	}

	if err := cap.DropBound(all...); err != nil {
		return fmt.Errorf("drop bounding set: %w", err)
	}

	if err := cap.ResetAmbient(); err != nil {
		return fmt.Errorf("reset ambient set: %w", err)
	}

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}

	if err := cap.NewSet().SetProc(); err != nil {
		return fmt.Errorf("clear capabilities: %w", err)
	}

	return nil
}
