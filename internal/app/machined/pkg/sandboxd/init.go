// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package sandboxd manages the sandbox PID+mount namespace in which machined's
// container-plane services (cri, kubelet, pods) run, isolated from machined.
package sandboxd

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/siderolabs/go-cmd/pkg/cmd/proc/reaper"
	"golang.org/x/sys/unix"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/siderolabs/talos/internal/pkg/selinux"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// Control protocol between machined (Client) and sandboxd.
//
// machined sends a launchRequest over the control socket as one SEQPACKET
// datagram, with SCM_RIGHTS carrying [stdin, stdout, stderr, serviceSocket].
// sandboxd - which is PID 1 inside the namespace - forks the service
// directly (no setns needed) and uses serviceSocket to (1) return a
// launchResponse plus the service pidfd via SCM_RIGHTS, and (2) after machined
// asks (Wait), return a waitResponse carrying the exit status.

// launchRequest is sent by machined for each service launch.
//
// SCM_RIGHTS carries [stdin, stdout, stderr, serviceSocket, extra files..., cgroup].
// The cgroup directory is present only when Cgroup is set.
type launchRequest struct {
	Args                []string
	Env                 []string
	DroppedCapabilities []string
	SelinuxLabel        string
	ExtraFiles          int
	Isolated            bool
	Cgroup              bool
	ReadOnlyPaths       []string
}

// maxLaunchFiles bounds the descriptors of one launch request.
const maxLaunchFiles = 16

// launchFiles are the descriptors received with one launch request.
type launchFiles struct {
	stdin, stdout, stderr, service *os.File
	extra                          []*os.File
	cgroup                         *os.File
}

func (f *launchFiles) all() []*os.File {
	files := append([]*os.File{f.stdin, f.stdout, f.stderr, f.service}, f.extra...)
	if f.cgroup != nil {
		files = append(files, f.cgroup)
	}

	return files
}

// launchResponse is returned once the service is started; the service pidfd is
// passed via SCM_RIGHTS alongside it. On error (Err != ""), no pidfd is sent.
type launchResponse struct {
	Err string
}

// waitResponse is returned after the service exits.
type waitResponse struct {
	ExitCode int
	ExitErr  string
}

const (
	// readyFD is the pipe sandboxd writes one byte to when ready.
	readyFD = 3
	// controlFD is the SEQPACKET socket machined sends launch requests on.
	controlFD = 4

	// oomScoreAdjInit is the oom_score_adj applied to sandboxd. It is PID 1
	// of the sandbox PID namespace: if the OOM killer reaps it, the kernel tears
	// down the whole namespace (cri, kubelet and every pod). It must therefore be a
	// last-resort victim; -1000 disables OOM killing for it. Services get their own
	// (higher) oom_score_adj applied by machined after launch.
	oomScoreAdjInit = -1000
)

// logf writes an operational log line to sandboxd's stderr. machined wires
// sandboxd's stdout/stderr to the logging manager, so these lines show up as the
// "sandboxd" service log (talosctl logs sandboxd).
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sandboxd: "+format+"\n", args...)
}

// setSelfOOMScoreAdj sets the calling process's oom_score_adj. Lowering it below
// 0 requires CAP_SYS_RESOURCE, which machined and its children hold.
func setSelfOOMScoreAdj(adj int) {
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte(strconv.Itoa(adj)), 0); err != nil {
		logf("failed to set oom_score_adj=%d: %v", adj, err)
	}
}

func ignoreTerminationSignals() func() {
	// Catch rather than SIG_IGN: exec resets caught signals to their defaults,
	// whereas ignored dispositions would leak into services and their children.
	// Notify installs the handlers synchronously and never blocks on delivery.
	// Intentionally leave the channel unconsumed to discard every notification.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)

	return func() { signal.Stop(signals) }
}

// Main is the entry point for the sandboxd subprocess. It is PID 1 of
// the sandbox PID+mount namespace and:
//  1. protects itself from the OOM killer and scopes /proc to this PID namespace,
//  2. catches and discards termination signals and reaps orphaned children,
//  3. serves launch requests from machined, forking each service directly into
//     this namespace (no setns).
func Main() {
	if len(os.Args) > 1 && os.Args[1] == isolateCommand {
		isolateMain(os.Args[2:])

		return
	}

	// Protect PID 1 of the sandbox namespace from the OOM killer before anything
	// else: its death collapses the entire namespace.
	setSelfOOMScoreAdj(oomScoreAdjInit)

	// Make /proc private so the overmount below stays in this mount namespace,
	// then remount /proc scoped to THIS PID namespace.
	if err := syscall.Mount("", "/proc", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		logf("make /proc private: %v", err)
		os.Exit(1)
	}

	if err := syscall.Mount("proc", "/proc", "proc",
		syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, ""); err != nil {
		logf("remount /proc: %v", err)
		os.Exit(1)
	}

	// Never let the control socket leak into a forked service.
	syscall.CloseOnExec(controlFD)

	// Wrap the control socket before starting the reaper, so a failure here can
	// os.Exit without skipping a deferred reaper.Shutdown().
	controlConn, err := unixConn(os.NewFile(controlFD, "sandboxd-control"))
	if err != nil {
		logf("wrap control socket: %v", err)
		os.Exit(1)
	}

	// As PID 1, catch and discard TERM/INT/HUP/QUIT so in-namespace `kill 1`
	// is a no-op without passing SIG_IGN to children. The kernel already blocks
	// namespace-local SIGKILL/SIGSTOP; machined can still tear us down via SIGKILL.
	// Install handlers before launching any children and release them on exit.
	stopSignals := ignoreTerminationSignals()
	defer stopSignals()

	// Reap orphaned children (e.g. containerd shims left behind when CRI is killed)
	// and the services this init forks.
	reaper.Run()

	defer reaper.Shutdown()

	// Signal machined that the namespace is up and the control socket is served.
	ready := os.NewFile(readyFD, "ready")
	ready.Write([]byte{1}) //nolint:errcheck
	ready.Close()          //nolint:errcheck

	logf("started as PID %d of the sandbox namespace; serving launch requests", os.Getpid())

	// Serve launch requests until machined closes the control socket (shutdown).
	for {
		req, fds, err := recvLaunch(controlConn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				logf("control socket closed; shutting down")

				return
			}

			logf("receiving launch request: %v", err)

			continue
		}

		go handleLaunch(req, fds)
	}
}

// recvLaunch reads one launch request datagram plus its SCM_RIGHTS fds from the control socket.
//
//nolint:gocyclo
func recvLaunch(control *net.UnixConn) (launchRequest, launchFiles, error) {
	var (
		req   launchRequest
		files launchFiles
	)

	buf := make([]byte, 64*1024) // request holds args+env; keep the SEQPACKET datagram whole
	oob := make([]byte, syscall.CmsgSpace(4*maxLaunchFiles))

	n, oobn, _, _, err := control.ReadMsgUnix(buf, oob)
	if err != nil {
		return req, files, err
	}

	if n == 0 {
		return req, files, io.EOF
	}

	scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return req, files, fmt.Errorf("parsing control message: %w", err)
	}

	var rawFds []int

	for i := range scms {
		fdSet, perr := syscall.ParseUnixRights(&scms[i])
		if perr != nil {
			return req, files, fmt.Errorf("parsing SCM_RIGHTS: %w", perr)
		}

		rawFds = append(rawFds, fdSet...)
	}

	closeRaw := func() {
		for _, fd := range rawFds {
			syscall.Close(fd) //nolint:errcheck
		}
	}

	if err := gob.NewDecoder(bytes.NewReader(buf[:n])).Decode(&req); err != nil {
		closeRaw()

		return req, files, fmt.Errorf("decoding launch request: %w", err)
	}

	expected := 4 + req.ExtraFiles
	if req.Cgroup {
		expected++
	}

	if req.ExtraFiles < 0 || len(rawFds) != expected {
		closeRaw()

		return req, files, fmt.Errorf("expected %d fds, got %d", expected, len(rawFds))
	}

	received := make([]*os.File, len(rawFds))

	for i, fd := range rawFds {
		// Received fds are not close-on-exec; mark them so nothing but the explicit
		// files (re-established via pa.Files) leaks into the forked service.
		syscall.CloseOnExec(fd)
		received[i] = os.NewFile(uintptr(fd), fmt.Sprintf("launch-fd-%d", i))
	}

	files.stdin, files.stdout, files.stderr, files.service = received[0], received[1], received[2], received[3]
	files.extra = received[4 : 4+req.ExtraFiles]

	if req.Cgroup {
		files.cgroup = received[len(received)-1]
	}

	return req, files, nil
}

// handleLaunch forks one service into the sandbox namespace and relays its
// pidfd and exit status back to machined over the per-service socket.
//
//nolint:gocyclo
func handleLaunch(req launchRequest, files launchFiles) {
	for _, file := range files.all() {
		if file != files.service {
			defer file.Close() //nolint:errcheck
		}
	}

	svcConn, err := unixConn(files.service)
	if err != nil {
		logf("wrap service socket: %v", err)

		return
	}

	defer svcConn.Close() //nolint:errcheck

	if len(req.Args) == 0 {
		sendLaunchErr(svcConn, "launch request has no arguments")

		return
	}

	launcher := cap.NewLauncher(req.Args[0], req.Args, req.Env)

	if req.Isolated {
		// The helper needs mount and capability privileges to build the
		// confinement; it drops every capability itself before exec.
		launcher = cap.NewLauncher("/proc/self/exe", isolateArgs(req), req.Env)
	} else if err := dropCaps(req.DroppedCapabilities, launcher); err != nil {
		sendLaunchErr(svcConn, fmt.Sprintf("drop capabilities for %s: %v", req.Args[0], err))

		return
	}

	launcher.Callback(beforeServiceExec)

	notifyCh := make(chan reaper.ProcessInfo, 8)
	reaper.Notify(notifyCh)

	defer reaper.Stop(notifyCh)

	pid, startErr := launcher.Launch(&launchContext{
		selinuxLabel: req.SelinuxLabel,
		files:        append([]*os.File{files.stdin, files.stdout, files.stderr}, files.extra...),
		isolated:     req.Isolated,
		cgroup:       files.cgroup,
	})
	if startErr != nil {
		sendLaunchErr(svcConn, fmt.Sprintf("exec %s: %v", req.Args[0], startErr))

		return
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		sendLaunchErr(svcConn, fmt.Sprintf("find process for %s: %v", req.Args[0], err))

		return
	}

	pidFd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		process.Kill()                                     //nolint:errcheck
		reaper.ProcessWaitWrapper(true, notifyCh, process) //nolint:errcheck
		sendLaunchErr(svcConn, fmt.Sprintf("open pidfd for %s: %v", req.Args[0], err))

		return
	}

	defer syscall.Close(pidFd) //nolint:errcheck

	// Start awaiting exit now (the reaper reaps our children), so an exit that
	// happens before machined calls Wait is captured.
	exitCh := make(chan error, 1)
	go func() {
		exitCh <- reaper.ProcessWaitWrapper(true, notifyCh, process)
	}()

	// Return the pidfd; machined resolves the host PID and signals through it.
	var respBuf bytes.Buffer
	if encErr := gob.NewEncoder(&respBuf).Encode(launchResponse{}); encErr != nil {
		logf("encode launch response: %v", encErr)
	}

	if _, _, err := svcConn.WriteMsgUnix(respBuf.Bytes(), syscall.UnixRights(pidFd), nil); err != nil {
		process.Kill() //nolint:errcheck
		<-exitCh

		return
	}

	logf("launched %s [pid %d]", req.Args[0], pid)

	// Wait for machined to ask (1 byte), then report the exit. If machined closes
	// the socket without asking, kill the service.
	var signalBuf [1]byte

	if _, err := svcConn.Read(signalBuf[:]); err != nil {
		process.Kill() //nolint:errcheck
		<-exitCh

		return
	}

	waitErr := <-exitCh

	resp := waitResponse{}

	if waitErr != nil {
		// A signaled process has no exit code; it is reported as -1.
		resp.ExitCode = -1
		resp.ExitErr = waitErr.Error()

		if exitErr, ok := errors.AsType[*reaper.ExitError](waitErr); ok {
			resp.ExitCode = exitErr.ExitCode
		}

		logf("%s [pid %d] exited: %v", req.Args[0], pid, waitErr)
	} else {
		logf("%s [pid %d] exited cleanly", req.Args[0], pid)
	}

	var waitBuf bytes.Buffer
	if encErr := gob.NewEncoder(&waitBuf).Encode(resp); encErr != nil {
		logf("encode wait response: %v", encErr)
	}

	svcConn.Write(waitBuf.Bytes()) //nolint:errcheck
}

// launchContext is passed to the libcap launch callback.
type launchContext struct {
	selinuxLabel string
	// files are the child's descriptors 0, 1, 2, ...
	files    []*os.File
	isolated bool
	cgroup   *os.File
}

// beforeServiceExec runs on libcap's launch thread just before it forks+execs
// the service. The init is already inside the sandbox namespace, so the child
// is created there directly (no setns).
func beforeServiceExec(pa *syscall.ProcAttr, data any) error {
	cfg, ok := data.(*launchContext)
	if !ok {
		return fmt.Errorf("failed to get sandbox launch context")
	}

	pa.Files = make([]uintptr, len(cfg.files))
	for i, file := range cfg.files {
		pa.Files[i] = file.Fd()
	}

	if cfg.isolated {
		isolateProcAttr(pa, cfg.cgroup)
	}

	if selinux.IsEnabled() {
		label := cfg.selinuxLabel
		if label == "" {
			label = constants.SelinuxLabelUnconfinedService
		}

		// The init is PID 1 of this namespace, so /proc/thread-self resolves in the
		// scoped /proc; write the pending exec label directly. It is consumed by the
		// execve, transitioning the service to its domain (e.g. pod_containerd_t).
		execAttr, err := os.OpenFile("/proc/thread-self/attr/exec", os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("opening SELinux exec label attr: %w", err)
		}

		defer execAttr.Close() //nolint:errcheck

		if err := writeFull(execAttr, []byte(label)); err != nil {
			return fmt.Errorf("setting SELinux exec label %q: %w", label, err)
		}
	}

	// Mark every fd >= 3 close-on-exec so the service inherits only the files
	// re-established via pa.Files above; the control socket, this launch's
	// service socket, other in-flight launches' fds and Go-runtime fds must not
	// leak into the service.
	if err := unix.CloseRange(3, math.MaxUint32, unix.CLOSE_RANGE_CLOEXEC); err != nil {
		return fmt.Errorf("close_range(CLOEXEC): %w", err)
	}

	return nil
}

// isolateProcAttr clones the helper into fresh namespaces and, when given,
// directly into its cgroup, so nothing it allocates is charged elsewhere.
func isolateProcAttr(pa *syscall.ProcAttr, cgroup *os.File) {
	if pa.Sys == nil {
		pa.Sys = &syscall.SysProcAttr{}
	}

	pa.Sys.Cloneflags = isolatedCloneFlags

	if cgroup != nil {
		pa.Sys.UseCgroupFD = true
		pa.Sys.CgroupFD = int(cgroup.Fd())
	}
}

func dropCaps(droppedCapabilities []string, launcher *cap.Launcher) error {
	dropped := make([]cap.Value, 0, len(droppedCapabilities))

	for _, name := range droppedCapabilities {
		capability, err := cap.FromName(name)
		if err != nil {
			return fmt.Errorf("parse capability %q: %w", name, err)
		}

		dropped = append(dropped, capability)
	}

	iab := cap.IABGetProc()
	if err := iab.SetVector(cap.Bound, true, dropped...); err != nil {
		return fmt.Errorf("set capability bounding set: %w", err)
	}

	launcher.SetIAB(iab)

	return nil
}

func sendLaunchErr(svcConn *net.UnixConn, errMsg string) {
	var buf bytes.Buffer

	if err := gob.NewEncoder(&buf).Encode(launchResponse{Err: errMsg}); err != nil {
		return
	}

	svcConn.Write(buf.Bytes()) //nolint:errcheck
}

// unixConn wraps an *os.File socket as a *net.UnixConn. FileConn duplicates the
// fd, so the original file is closed.
func unixConn(f *os.File) (*net.UnixConn, error) {
	conn, err := net.FileConn(f)
	f.Close() //nolint:errcheck

	if err != nil {
		return nil, err
	}

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close() //nolint:errcheck

		return nil, fmt.Errorf("not a unix socket")
	}

	return uc, nil
}
