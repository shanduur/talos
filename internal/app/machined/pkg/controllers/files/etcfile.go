// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/siderolabs/talos/internal/pkg/mount/v2"
	"github.com/siderolabs/talos/internal/pkg/selinux"
	"github.com/siderolabs/talos/internal/pkg/xfs"
	"github.com/siderolabs/talos/pkg/machinery/resources/files"
)

// EtcFileController watches EtcFileSpecs, creates/updates files.
type EtcFileController struct {
	// Path to /etc directory, read-only filesystem.
	EtcPath string

	// AnonFS is a writable filesystem that is used to create bind mounts.
	AnonFS xfs.FS

	// Cache of bind mounts created.
	bindMounts map[string]struct{}
}

// Name implements controller.Controller interface.
func (ctrl *EtcFileController) Name() string {
	return "files.EtcFileController"
}

// Inputs implements controller.Controller interface.
func (ctrl *EtcFileController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: files.NamespaceName,
			Type:      files.EtcFileSpecType,
			Kind:      controller.InputStrong,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *EtcFileController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: files.EtcFileStatusType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
//
//nolint:gocyclo,cyclop
func (ctrl *EtcFileController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	if ctrl.bindMounts == nil {
		ctrl.bindMounts = make(map[string]struct{})
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		list, err := safe.ReaderList[*files.EtcFileSpec](ctx, r, resource.NewMetadata(files.NamespaceName, files.EtcFileSpecType, "", resource.VersionUndefined))
		if err != nil {
			return fmt.Errorf("error listing specs: %w", err)
		}

		// add finalizers for all live resources
		for res := range list.All() {
			if res.Metadata().Phase() != resource.PhaseRunning {
				continue
			}

			if err = r.AddFinalizer(ctx, res.Metadata(), ctrl.Name()); err != nil {
				return fmt.Errorf("error adding finalizer: %w", err)
			}
		}

		touchedIDs := make(map[resource.ID]struct{})

		for spec := range list.All() {
			filename := spec.Metadata().ID()
			_, mountExists := ctrl.bindMounts[filename]

			dst := filepath.Join(ctrl.EtcPath, filename)
			src := dst

			switch spec.Metadata().Phase() {
			case resource.PhaseTearingDown:
				if mountExists {
					logger.Debug("removing bind mount", zap.String("src", src), zap.String("dst", dst))

					if err = unix.Unmount(dst, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
						return fmt.Errorf("failed to unmount bind mount %q: %w", dst, err)
					}

					delete(ctrl.bindMounts, filename)
				}

				logger.Debug("removing file", zap.String("src", src))

				if err = os.Remove(src); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("failed to remove %q: %w", src, err)
				}

				// now remove finalizer as the link was deleted
				if err = r.RemoveFinalizer(ctx, spec.Metadata(), ctrl.Name()); err != nil {
					return fmt.Errorf("error removing finalizer: %w", err)
				}
			case resource.PhaseRunning:
				if !mountExists {
					logger.Debug("creating bind mount", zap.String("src", src), zap.String("dst", dst))

					if err = createBindMountFile(ctrl.AnonFS, src, dst, spec.TypedSpec().Mode); err != nil {
						return fmt.Errorf("failed to create shadow bind mount %q -> %q: %w", src, dst, err)
					}

					ctrl.bindMounts[filename] = struct{}{}
				}

				logger.Debug("writing file contents", zap.String("src", src), zap.Stringer("version", spec.Metadata().Version()))

				if err = UpdateFileFs(ctrl.AnonFS, src, spec.TypedSpec().Contents, spec.TypedSpec().Mode, spec.TypedSpec().SelinuxLabel); err != nil {
					return fmt.Errorf("error updating %q: %w", src, err)
				}

				if err = safe.WriterModify(ctx, r, files.NewEtcFileStatus(files.NamespaceName, filename), func(r *files.EtcFileStatus) error {
					r.TypedSpec().SpecVersion = spec.Metadata().Version().String()

					return nil
				}); err != nil {
					return fmt.Errorf("error updating status: %w", err)
				}

				touchedIDs[filename] = struct{}{}
			}
		}

		// list statuses for cleanup
		statuses, err := safe.ReaderList[*files.EtcFileStatus](ctx, r, resource.NewMetadata(files.NamespaceName, files.EtcFileStatusType, "", resource.VersionUndefined))
		if err != nil {
			return fmt.Errorf("error listing resources: %w", err)
		}

		for res := range statuses.All() {
			if _, ok := touchedIDs[res.Metadata().ID()]; !ok {
				if err = r.Destroy(ctx, res.Metadata()); err != nil {
					return fmt.Errorf("error cleaning up specs: %w", err)
				}
			}
		}

		r.ResetRestartBackoff()
	}
}

// createBindMountFile creates a common way to create a writable source file with a
// bind mounted destination. This is most commonly used for well known files
// under /etc that need to be adjusted during startup.
func createBindMountFile(anonfs xfs.FS, src, dst string, mode os.FileMode) (err error) {
	if err = xfs.MkdirAll(anonfs, filepath.Dir(src), 0o755); err != nil {
		return err
	}

	var f fs.File

	if f, err = xfs.OpenFile(anonfs, src, os.O_WRONLY|os.O_CREATE, mode); err != nil {
		return err
	}

	if err = f.Close(); err != nil {
		return err
	}

	return mount.BindReadonly(filepath.Join(anonfs.MountPoint(), src), dst)
}

// createBindMountDir creates a common way to create a writable source dir with a
// bind mounted destination. This is most commonly used for well known directories
// under /etc that need to be adjusted during startup.
func createBindMountDirFs(anonfs xfs.FS, src, dst string) error {
	err := xfs.MkdirAll(anonfs, src, 0o755)
	if err != nil {
		return err
	}

	return mount.BindReadonly(filepath.Join(anonfs.MountPoint(), src), dst)
}

// UpdateFile is like `os.WriteFile`, but it will only update the file if the
// contents have changed.
func UpdateFile(filename string, contents []byte, mode os.FileMode, selinuxLabel string) error {
	oldContents, err := os.ReadFile(filename)
	if err == nil && bytes.Equal(oldContents, contents) {
		return selinux.SetLabel(filename, selinuxLabel)
	}

	err = os.WriteFile(filename, contents, mode)
	if err != nil {
		return err
	}

	return selinux.SetLabel(filename, selinuxLabel)
}

// UpdateFileFs is like `os.WriteFile`, but it will only update the file if the
// contents have changed. It acts on a file in the anonymous filesystem.
func UpdateFileFs(anonfs xfs.FS, filename string, contents []byte, mode os.FileMode, selinuxLabel string) error {
	oldContents, err := xfs.ReadFile(anonfs, filename)
	if err == nil && bytes.Equal(oldContents, contents) {
		return selinux.SetLabel(filename, selinuxLabel)
	}

	err = xfs.WriteFile(anonfs, filename, contents, mode)
	if err != nil {
		return err
	}

	return selinux.SetLabel(filename, selinuxLabel)
}
