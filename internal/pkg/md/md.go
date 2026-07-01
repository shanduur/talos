// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package md provides a Go interface to Linux MD (software RAID) arrays via
// the mdadm(8) utility.
//
// mdadm ships in a system extension and is NOT part of core Talos. New()
// resolves the binary up-front and returns ErrNotInstalled when it is
// missing, so callers can surface a precondition failure instead of an
// opaque exec error.
package md

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/siderolabs/go-cmd/pkg/cmd"
)

// defaultMdadmPaths lists the locations mdadm is installed to, in order of
// preference. The system extension installs into /usr/local/sbin; the bare
// name is the last resort so an operator-provided PATH still works.
var defaultMdadmPaths = []string{
	"/usr/local/sbin/mdadm",
	"/sbin/mdadm",
	"mdadm",
}

// MD provides methods for managing MD (software RAID) arrays.
type MD struct {
	mdadm string
}

// New creates a new MD instance, resolving the mdadm binary.
//
// Returns ErrNotInstalled if mdadm cannot be found, because mdadm is provided
// by a system extension rather than core Talos.
func New(opts ...Option) (*MD, error) {
	md := &MD{}

	for _, opt := range opts {
		opt(md)
	}

	if md.mdadm == "" {
		md.mdadm = resolveMdadm()
	}

	if md.mdadm == "" {
		return nil, ErrNotInstalled
	}

	return md, nil
}

// resolveMdadm returns the first mdadm binary that exists, or "" if none.
func resolveMdadm() string {
	for _, p := range defaultMdadmPaths {
		if resolved, err := exec.LookPath(p); err == nil {
			return resolved
		}
	}

	return ""
}

// Option is a functional option for configuring the MD instance.
type Option func(*MD)

// WithMdadmPath sets an explicit path to the mdadm binary.
func WithMdadmPath(path string) Option {
	return func(md *MD) {
		md.mdadm = path
	}
}

// run executes `mdadm <args...>` and returns stdout.
//
// Errors are normalised through classifyError so every caller sees the same
// sentinel set (ErrNotFound, ErrInUse, ErrExists, ErrCommand). The raw mdadm
// stderr is kept out of the returned error chain — only the sentinel is
// wrapped — so it will not be surfaced to API clients by mistake.
func (md *MD) run(ctx context.Context, args ...string) (string, error) {
	out, err := cmd.RunWithOptions(ctx, md.mdadm, args, cmd.WithFullStdoutCapture())
	if err != nil {
		// An ENOENT here means the binary vanished between New and run; map it
		// back to the precondition sentinel rather than a generic command error.
		if errors.Is(err, exec.ErrNotFound) {
			return "", ErrNotInstalled
		}

		return "", fmt.Errorf("mdadm failed: %w", classifyError(err))
	}

	return out, nil
}
