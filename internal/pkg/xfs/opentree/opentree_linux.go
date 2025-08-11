// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package opentree provides a simple interface to create and manage a subfilesystem
// using the `open_tree` syscall. It allows for creating a new subfilesystem
// by cloning an existing filesystem tree and provides a method to close the filesystem
// when it is no longer needed.
package opentree

import (
	"fmt"
	"sync"

	"golang.org/x/sys/unix"

	xfs "github.com/siderolabs/talos/internal/pkg/xfs"
)

// FS represents a subfilesystem that can be created and managed.
// It uses a file descriptor to represent the mounted filesystem and provides methods
// to create and close the filesystem. The creation of the filesystem is idempotent,
// meaning it can be called multiple times without side effects.
type FS struct {
	createOnce  sync.Once
	createError error

	root   string
	rootfd int

	mntfd int
}

// Interface guard.
var _ xfs.Creator = (*FS)(nil)

// NewFromPath creates a new fs instance from path.
func NewFromPath(path string) *FS {
	return &FS{
		root:   path,
		rootfd: unix.AT_FDCWD,
	}
}

// NewFromFd creates a new fs instance from file descriptor.
func NewFromFd(fd int) *FS {
	return &FS{
		rootfd: fd,
	}
}

// Create initializes the fs filesystem and returns the file descriptor for the mounted filesystem.
// If the filesystem is already created, it returns the existing file descriptor.
// This method is idempotent, meaning it can be called multiple times without side effects.
func (fs *FS) Create() (int, error) {
	fs.createOnce.Do(func() {
		flags := unix.OPEN_TREE_CLONE | unix.OPEN_TREE_CLOEXEC
		if fs.root == "" {
			flags |= unix.AT_EMPTY_PATH
		}

		fs.createError = fs.new(uint(flags))
	})

	return fs.mntfd, fs.createError
}

func (fs *FS) new(flags uint) error {
	mntfd, err := unix.OpenTree(fs.rootfd, fs.root, flags)
	if err != nil {
		return fmt.Errorf("unix.OpenTree on %q failed: %w", fs.root, err)
	}

	fs.mntfd = mntfd

	return nil
}

// Close unmounts the fs filesystem and closes the file descriptor.
func (fs *FS) Close() error {
	return unix.Close(fs.mntfd)
}
