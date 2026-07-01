// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package md

import (
	"errors"
	"regexp"

	"github.com/siderolabs/go-cmd/pkg/cmd"
)

// Sentinel errors returned by the package. Callers should use errors.Is to
// dispatch on these rather than parsing the underlying mdadm output, which is
// human-oriented and may change between mdadm releases.
var (
	// ErrNotInstalled is returned when the mdadm binary is not present on the
	// node. mdadm ships in a system extension and is NOT part of core Talos,
	// so every RPC must be prepared to report this as a precondition failure.
	ErrNotInstalled = errors.New("md: mdadm is not installed (requires the mdadm system extension)")
	// ErrNotFound is returned when the target array or member device does not
	// exist.
	ErrNotFound = errors.New("md: array not found")
	// ErrInUse is returned when the array or a member device is busy (mounted,
	// held open, or claimed by another device).
	ErrInUse = errors.New("md: resource in use")
	// ErrExists is returned when a create operation targets a device that is
	// already part of a RAID array, or an array name that already exists.
	ErrExists = errors.New("md: array or member already exists")
	// ErrInvalidArgument is returned when mdadm rejected the command line
	// (bad flags, wrong device count for the level, ...).
	ErrInvalidArgument = errors.New("md: invalid argument")
	// ErrCommand is returned for any non-zero exit that does not match a more
	// specific sentinel.
	ErrCommand = errors.New("md: command failed")
)

// stderr matchers derived from the mdadm source (Manage.c, Create.c, util.c).
// Patterns are deliberately narrow: a false positive that misclassifies a
// transient failure is worse than collapsing to ErrCommand.
var (
	// "mdadm: cannot open /dev/mdX: No such file or directory"
	// "mdadm: /dev/mdX does not appear to be an md device"
	// "mdadm: cannot find /dev/...: No such file or directory"
	notFoundRE = regexp.MustCompile(`(No such file or directory|does not appear to be an md device|cannot find)`)

	// "mdadm: Cannot get exclusive access to /dev/mdX:..."
	// "mdadm: /dev/sdX is busy - skipping"
	// "Device or resource busy"
	inUseRE = regexp.MustCompile(`(Cannot get exclusive access|is busy|Device or resource busy)`)

	// "mdadm: /dev/sdX appears to be part of a raid array"
	// "mdadm: /dev/sdX is already in use"
	// "mdadm: device /dev/mdX already active"
	existsRE = regexp.MustCompile(`(appears to be part of a raid array|already in use|already active|already exists)`)
)

// ExecError carries the classified sentinel together with the raw exit code
// and stderr from a failed mdadm invocation. It is returned by (*MD).run when
// the command exits non-zero.
//
// The structured fields let server-side handlers log the full diagnostic
// without surfacing the raw mdadm output to the API client — Error() only
// renders the sentinel for that reason.
type ExecError struct {
	Sentinel error
	ExitCode int
	Stderr   []byte
}

// Error implements the error interface. Returns only the sentinel message so
// raw mdadm stderr is never leaked through err.Error().
func (e *ExecError) Error() string {
	return e.Sentinel.Error()
}

// Unwrap returns the sentinel so errors.Is/As routes through it.
func (e *ExecError) Unwrap() error {
	return e.Sentinel
}

// classifyError maps a *cmd.ExitError to an *ExecError carrying the matching
// sentinel together with the raw exit code and stderr. Non-ExitError inputs
// (context cancellation, exec failures) are returned untouched so callers can
// still use errors.Is(err, context.Canceled).
func classifyError(err error) error {
	var exit *cmd.ExitError

	if !errors.As(err, &exit) {
		return err
	}

	return &ExecError{
		Sentinel: sentinelFor(exit),
		ExitCode: exit.ExitCode,
		Stderr:   exit.Output,
	}
}

// sentinelFor picks the most specific sentinel for an mdadm ExitError using
// stderr matchers, falling back to ErrCommand.
func sentinelFor(exit *cmd.ExitError) error {
	switch {
	case existsRE.Match(exit.Output):
		return ErrExists
	case inUseRE.Match(exit.Output):
		return ErrInUse
	case notFoundRE.Match(exit.Output):
		return ErrNotFound
	}

	return ErrCommand
}
