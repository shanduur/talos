// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package sandboxd_test

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/internal/app/machined/pkg/sandboxd"
)

// mainHelperEnv makes the test binary run as sandboxd. sandboxd re-executes
// its own binary for isolated launches, which keeps this variable set.
const mainHelperEnv = "SANDBOXD_TEST_MAIN=1"

func TestMain(m *testing.M) {
	if slices.Contains(os.Environ(), mainHelperEnv) {
		sandboxd.Main()
		os.Exit(0)
	}

	os.Exit(m.Run())
}

func TestIsolateRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"isolate"},
		{"isolate", "1", "--"},
		{"isolate", "x", "--", "/bin/true"},
		{"isolate", "-1", "--", "/bin/true"},
		{"isolate", "0", "relative", "--", "/bin/true"},
		{"isolate", "0", "/", "--", "/bin/true"},
		{"isolate", "0", "/usr/../etc", "--", "/bin/true"},
		{"isolate", "0", "/run/secret", "--", "/bin/true"},
	} {
		cmd := exec.CommandContext(t.Context(), os.Args[0])

		cmd.Args = append([]string{"sandboxd"}, args...)
		cmd.Env = append(os.Environ(), mainHelperEnv)

		output, err := cmd.CombinedOutput()

		exitErr, ok := errors.AsType[*exec.ExitError](err)
		require.True(t, ok, "%q: %v %s", args, err, output)
		require.Equal(t, 126, exitErr.ExitCode(), "%q: %s", args, output)
		require.Contains(t, string(output), "sandboxd isolate:", "%q", args)
	}
}

// startSandboxd runs sandboxd as PID 1 of an unprivileged user, PID and mount
// namespace: the same launch and confinement steps as on a node, without cgroups.
func startSandboxd(t *testing.T) *sandboxd.Client {
	t.Helper()

	readyR, readyW, err := os.Pipe()
	require.NoError(t, err)

	defer readyR.Close() //nolint:errcheck

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	require.NoError(t, err)

	controlInit := os.NewFile(uintptr(fds[1]), "control-init")

	var stderr bytes.Buffer

	cmd := exec.CommandContext(t.Context(), os.Args[0])
	cmd.Args[0] = "sandboxd"

	cmd.Env = append(os.Environ(), mainHelperEnv)
	cmd.ExtraFiles = []*os.File{readyW, controlInit}
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}

	err = cmd.Start()

	readyW.Close()      //nolint:errcheck
	controlInit.Close() //nolint:errcheck

	if err != nil {
		t.Skipf("unprivileged user namespaces unavailable: %v", err)
	}

	control, err := net.FileConn(os.NewFile(uintptr(fds[0]), "control-machined"))
	require.NoError(t, err)

	t.Cleanup(func() {
		control.Close() //nolint:errcheck
		cmd.Wait()      //nolint:errcheck
	})

	if _, err = readyR.Read(make([]byte, 1)); err != nil {
		// Build containers may forbid mounting a procfs over a masked /proc.
		t.Skipf("sandboxd cannot start unprivileged here: %v %s", err, stderr.String())
	}

	unixControl, ok := control.(*net.UnixConn)
	require.True(t, ok)

	return sandboxd.NewClient(unixControl)
}

func TestIsolatedLaunchConfinesCommand(t *testing.T) {
	client := startSandboxd(t)

	shell, err := exec.LookPath("sh")
	require.NoError(t, err)

	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	destination := filepath.Join(dir, "destination")

	require.NoError(t, os.WriteFile(source, []byte("image bytes"), 0o600))
	require.NoError(t, os.WriteFile(destination, nil, 0o600))

	input, err := os.Open(source)
	require.NoError(t, err)

	defer input.Close() //nolint:errcheck

	output, err := os.OpenFile(destination, os.O_RDWR, 0)
	require.NoError(t, err)

	defer output.Close() //nolint:errcheck

	// Everything the shell needs on a typical host, without nesting.
	var (
		readOnly []string
		entries  = []string{"dev", "proc"}
	)

	for _, path := range []string{"/bin", "/usr", "/lib", "/lib64", "/etc/ld.so.cache"} {
		if _, statErr := os.Stat(path); statErr == nil {
			readOnly = append(readOnly, path)
			entries = append(entries, strings.SplitN(path[1:], "/", 2)[0])
		}
	}

	slices.Sort(entries)

	script := `set -e
test "$(ls -A / | sort | tr '\n' ' ')" = "$EXPECTED"
test "$$" = 1
grep -q '^CapEff:	0000000000000000$' /proc/self/status
grep -q '^CapBnd:	0000000000000000$' /proc/self/status
grep -q '^NoNewPrivs:	1$' /proc/self/status
! touch /escape 2>/dev/null
! ls /proc/self/fd/5 2>/dev/null
cat /proc/self/fd/3 > /proc/self/fd/4
echo confined`

	stdout, err := os.Create(filepath.Join(dir, "stdout"))
	require.NoError(t, err)

	defer stdout.Close() //nolint:errcheck

	handle, err := client.Launch(runtime.LaunchConfig{
		Args:       []string{shell, "-c", script},
		Env:        []string{mainHelperEnv, "EXPECTED=" + strings.Join(entries, " ") + " "},
		Stdout:     stdout,
		Stderr:     stdout,
		ExtraFiles: []*os.File{input, output},
		Isolation:  &runtime.Isolation{ReadOnlyPaths: readOnly},
	})
	require.NoError(t, err)

	defer handle.Close() //nolint:errcheck

	code, err := handle.Wait()

	logged, readErr := os.ReadFile(stdout.Name())
	require.NoError(t, readErr)
	require.NoError(t, err, "%s", logged)
	require.Zero(t, code)
	require.Contains(t, string(logged), "confined")

	copied, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, "image bytes", string(copied), "passed descriptors are reachable through /proc/self/fd")
}

func TestLaunchReportsExitCode(t *testing.T) {
	client := startSandboxd(t)

	shell, err := exec.LookPath("sh")
	require.NoError(t, err)

	handle, err := client.Launch(runtime.LaunchConfig{
		Args:   []string{shell, "-c", "exit 7"},
		Stdout: os.Stderr,
		Stderr: os.Stderr,
	})
	require.NoError(t, err)

	defer handle.Close() //nolint:errcheck

	code, err := handle.Wait()
	require.Error(t, err)
	require.Equal(t, 7, code)
}
