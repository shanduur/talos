// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/opencontainers/go-digest"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/internal/cleanup"
	"github.com/siderolabs/talos/pkg/machinery/hypervisorhelpers"
	"github.com/siderolabs/talos/pkg/machinery/resources/hypervisor"
	"github.com/siderolabs/talos/pkg/machinery/resources/storage"
	"github.com/siderolabs/talos/pkg/machinery/storagehelpers"
)

// rawDiskFormat is libvirt's driver type for a file attached as-is, which is how a cdrom's image is
// presented. A blank disk takes its driver type from the volume that was made for it.
const rawDiskFormat = "raw"

const diskControllerName = "hypervisor.VirtualMachineDiskController"

// blankDiskFormats are the formats a volume can be made in: the enum's members less its zero one.
//
// Not VirtualMachineDiskFormatStrings(), and not VirtualMachineDiskFormatString(): enumer puts the
// zero member in its name map, so both accept "unknown", which names no format and which libvirt
// would be handed verbatim.
var blankDiskFormats = []string{
	hypervisorhelpers.VirtualMachineDiskFormatRaw.String(),
	hypervisorhelpers.VirtualMachineDiskFormatQCOW2.String(),
}

// errHoldFailed marks the controller's own failure to hold a library.
var errHoldFailed = errors.New("failed to hold content library")

// VirtualMachineDiskController resolves each disk of a virtual machine to a host source.
type VirtualMachineDiskController struct{}

// Name implements controller.Controller interface.
func (ctrl *VirtualMachineDiskController) Name() string {
	return diskControllerName
}

// Inputs implements controller.Controller interface.
func (ctrl *VirtualMachineDiskController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: hypervisor.NamespaceName,
			Type:      hypervisor.VirtualMachineSpecType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: hypervisor.NamespaceName,
			Type:      hypervisor.ContentLibraryStatusType,
			// Strong: an image is attached where it lies, so a library's mount has to outlive every guest reading one.
			Kind: controller.InputStrong,
		},
		{
			Namespace: hypervisor.NamespaceName,
			Type:      hypervisor.VirtualMachineDiskStatusType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: storage.NamespaceName,
			Type:      storage.StoragePoolVolumeStatusType,
			// Weak: the volume is held through the spec authored for it, not through its status.
			Kind: controller.InputWeak,
		},
		{
			Namespace: storage.NamespaceName,
			Type:      storage.StoragePoolVolumeSpecType,
			Kind:      controller.InputDestroyReady,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *VirtualMachineDiskController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: hypervisor.VirtualMachineDiskStatusType,
			Kind: controller.OutputExclusive,
		},
		{
			// Shared, and in another slice's namespace: the volume is asked for here and made by
			// the storage slice, which owns the storage daemon. The same shape as a content library
			// asking the block slice for its mount.
			Type: storage.StoragePoolVolumeSpecType,
			Kind: controller.OutputShared,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *VirtualMachineDiskController) Run(ctx context.Context, runtime controller.Runtime, logger *zap.Logger) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-runtime.EventCh():
		}

		if err := ctrl.reconcile(ctx, runtime, logger); err != nil {
			return err
		}

		runtime.ResetRestartBackoff()
	}
}

func (ctrl *VirtualMachineDiskController) reconcile(ctx context.Context, r controller.ReaderWriter, logger *zap.Logger) error {
	specs, err := safe.ReaderListAll[*hypervisor.VirtualMachineSpec](ctx, r)
	if err != nil {
		return fmt.Errorf("failed to list virtual machine specs: %w", err)
	}

	libraryStatuses, err := safe.ReaderListAll[*hypervisor.ContentLibraryStatus](ctx, r)
	if err != nil {
		return fmt.Errorf("failed to list content library statuses: %w", err)
	}

	libraries := make(map[string]*hypervisor.ContentLibraryStatus, libraryStatuses.Len())

	for library := range libraryStatuses.All() {
		libraries[library.Metadata().ID()] = library
	}

	wanted := make(map[resource.ID]struct{}, specs.Len())
	held := map[string]struct{}{}
	volumes := map[resource.ID]struct{}{}

	var errs []error

	for vm := range specs.All() {
		name := vm.Metadata().ID()

		for _, disk := range vm.TypedSpec().Disks {
			id := hypervisor.VirtualMachineDiskStatusID(name, disk)
			wanted[id] = struct{}{}

			if err := ctrl.reconcileDisk(ctx, r, logger, name, id, disk, libraries, held, volumes); err != nil {
				errs = append(errs, err)
			}
		}
	}

	// A status the configuration has dropped is torn down to ask VirtualMachineController for its hold back.
	if err := cleanup.Outputs[*hypervisor.VirtualMachineDiskStatus](ctx, r, "virtual machine disk status", wanted); err != nil {
		return errors.Join(append(errs, err)...)
	}

	if err := ctrl.releaseVolumeSpecs(ctx, r, volumes); err != nil {
		return errors.Join(append(errs, err)...)
	}

	return errors.Join(append(errs, ctrl.releaseLibraries(ctx, r, logger, libraries, held))...)
}

// releaseVolumeSpecs withdraws the volume of every blank disk nothing needs any more.
//
// Kept as well as wanted are the volumes named by a disk status something holds: a running domain
// has those open, and withdrawing the spec would release the pool -- and with it the mount -- from
// under it. The configuration no longer naming a disk is not the same as a guest having let go of
// it. Nothing here deletes a volume; the file outlives its spec by design.
func (ctrl *VirtualMachineDiskController) releaseVolumeSpecs(
	ctx context.Context, r controller.ReaderWriter, volumes map[resource.ID]struct{},
) error {
	diskStatuses, err := safe.ReaderListAll[*hypervisor.VirtualMachineDiskStatus](ctx, r)
	if err != nil {
		return fmt.Errorf("failed to list virtual machine disk statuses: %w", err)
	}

	keep := maps.Clone(volumes)

	for diskStatus := range diskStatuses.All() {
		if diskStatus.Metadata().Finalizers().Empty() {
			continue
		}

		spec := diskStatus.TypedSpec()

		if spec.Pool != "" && spec.Volume != "" {
			keep[storage.StoragePoolVolumeID(spec.Pool, spec.Volume)] = struct{}{}
		}
	}

	return cleanup.Outputs[*storage.StoragePoolVolumeSpec](ctx, r, "storage pool volume spec", keep)
}

// reconcileDisk publishes the status of one disk of one virtual machine.
func (ctrl *VirtualMachineDiskController) reconcileDisk(
	ctx context.Context,
	r controller.ReaderWriter,
	logger *zap.Logger,
	name string,
	id resource.ID,
	disk hypervisor.VirtualMachineDiskSpec,
	libraries map[string]*hypervisor.ContentLibraryStatus,
	held map[string]struct{},
	volumes map[resource.ID]struct{},
) error {
	// A status of this exact disk may still be tearing down, held by a domain reading from it:
	// nothing can be written to it, and downstream reads it as absent.
	switch existing, err := safe.ReaderGetByID[*hypervisor.VirtualMachineDiskStatus](ctx, r, id); {
	case err != nil && !state.IsNotFoundError(err):
		return fmt.Errorf("failed to get virtual machine disk status %q: %w", id, err)
	case err == nil && existing.Metadata().Phase() != resource.PhaseRunning:
		return nil
	}

	// The library is held before the status resolved against it is published: a ready disk is one
	// something may start using at any moment.
	resolved, resolveErr := ctrl.resolve(ctx, r, logger, name, disk, libraries, held, volumes)
	if errors.Is(resolveErr, errHoldFailed) {
		return fmt.Errorf("failed to resolve virtual machine disk %q: %w", id, resolveErr)
	}

	if err := safe.WriterModify(ctx, r,
		hypervisor.NewVirtualMachineDiskStatus(hypervisor.NamespaceName, id),
		func(res *hypervisor.VirtualMachineDiskStatus) error {
			stampDiskStatus(res.TypedSpec(), name, disk, resolved, resolveErr)

			return nil
		},
	); err != nil {
		return fmt.Errorf("failed to write virtual machine disk status %q: %w", id, err)
	}

	return nil
}

// stampDiskStatus fills in what a disk status says about itself, resolved or not.
//
// What the disk was for is stamped outside the resolution, so a status which did not resolve still
// names its image and its volume. The volume matters doubly: it is what tells a pool its volume is
// still in use, and a status which did not resolve has no source path to go on.
func stampDiskStatus(
	status *hypervisor.VirtualMachineDiskStatusSpec,
	name string,
	disk hypervisor.VirtualMachineDiskSpec,
	resolved hypervisor.VirtualMachineDiskStatusSpec,
	resolveErr error,
) {
	*status = resolved
	status.VirtualMachine = name
	status.Name = disk.Name

	if image := disk.Provision.FromImage; image != nil {
		status.Image = *image
	}

	if disk.Provision.Blank {
		status.Blank = true
		status.Pool = disk.Pool

		if volumeName, err := blankVolumeName(name, disk); err == nil {
			status.Volume = volumeName
		}
	}

	if resolveErr != nil {
		status.Error = resolveErr.Error()
	}
}

// holdLibrary keeps a library's mount in place for as long as a disk resolves against it. One which
// is tearing down is refused rather than held, as holding it now would block that teardown forever.
func holdLibrary(
	ctx context.Context, r controller.ReaderWriter, logger *zap.Logger, library *hypervisor.ContentLibraryStatus,
) error {
	if library.Metadata().Phase() != resource.PhaseRunning {
		return fmt.Errorf("content library %q is going away", library.Metadata().ID())
	}

	if library.Metadata().Finalizers().Has(diskControllerName) {
		return nil
	}

	if err := r.AddFinalizer(ctx, library.Metadata(), diskControllerName); err != nil {
		return fmt.Errorf("%w %q: %w", errHoldFailed, library.Metadata().ID(), err)
	}

	logger.Info("holding content library for a virtual machine disk", zap.String("content_library", library.Metadata().ID()))

	return nil
}

// releaseLibraries gives back the hold on every library nothing resolves against any more.
func (ctrl *VirtualMachineDiskController) releaseLibraries(
	ctx context.Context, r controller.ReaderWriter, logger *zap.Logger,
	libraries map[string]*hypervisor.ContentLibraryStatus, held map[string]struct{},
) error {
	inUse, err := librariesInUse(ctx, r, held)
	if err != nil {
		return err
	}

	for id, library := range libraries {
		if _, used := inUse[id]; used || !library.Metadata().Finalizers().Has(diskControllerName) {
			continue
		}

		if err := r.RemoveFinalizer(ctx, library.Metadata(), diskControllerName); err != nil && !state.IsNotFoundError(err) {
			return fmt.Errorf("failed to release content library %q: %w", id, err)
		}

		logger.Info("released content library held for a virtual machine disk", zap.String("content_library", id))
	}

	return nil
}

// librariesInUse names the libraries which must stay held, by ID: the ones held names, plus every
// library named by a disk status something else holds.
func librariesInUse(ctx context.Context, reader controller.Reader, held map[string]struct{}) (map[string]struct{}, error) {
	diskStatuses, err := safe.ReaderListAll[*hypervisor.VirtualMachineDiskStatus](ctx, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to list virtual machine disk statuses: %w", err)
	}

	inUse := maps.Clone(held)

	for diskStatus := range diskStatuses.All() {
		if diskStatus.Metadata().Finalizers().Empty() {
			continue
		}

		if library := diskStatus.TypedSpec().Image.Library; library != "" {
			inUse[library] = struct{}{}
		}
	}

	return inUse, nil
}

// resolve finds the host source for a disk, holding the library it resolved against first and
// recording it in held so the same pass does not give it straight back.
func (ctrl *VirtualMachineDiskController) resolve(
	ctx context.Context,
	r controller.ReaderWriter,
	logger *zap.Logger,
	name string,
	disk hypervisor.VirtualMachineDiskSpec,
	libraries map[string]*hypervisor.ContentLibraryStatus,
	held map[string]struct{},
	volumes map[resource.ID]struct{},
) (hypervisor.VirtualMachineDiskStatusSpec, error) {
	if err := checkVirtualMachineDiskSupported(disk); err != nil {
		return hypervisor.VirtualMachineDiskStatusSpec{}, err
	}

	if disk.Provision.Blank {
		return ctrl.resolveBlankDisk(ctx, r, name, disk, volumes)
	}

	image := disk.Provision.FromImage

	library, found := libraries[image.Library]
	if !found {
		return hypervisor.VirtualMachineDiskStatusSpec{}, fmt.Errorf("content library %q is not configured", image.Library)
	}

	if err := holdLibrary(ctx, r, logger, library); err != nil {
		return hypervisor.VirtualMachineDiskStatusSpec{}, err
	}

	held[image.Library] = struct{}{}

	return resolveVirtualMachineDisk(disk, *library.TypedSpec())
}

// checkVirtualMachineDiskSupported reports whether a disk is one this slice provisions at all, as
// opposed to one that is merely not resolved yet. Kept apart so the status and the render grade it alike.
//
// Machine configuration validation already rejects most of this. It is checked again because
// VirtualMachineSpec is a shared output: another producer may author one, and nothing binds it to
// the rules a machine configuration document is held to.
func checkVirtualMachineDiskSupported(disk hypervisor.VirtualMachineDiskSpec) error {
	switch disk.Type {
	case hypervisorhelpers.VirtualMachineDiskTypeCDROM.String():
		switch {
		case disk.Provision.Blank:
			return fmt.Errorf("%w: a cdrom has no contents of its own", errDiskUnsupported)
		case disk.Provision.FromImage == nil:
			return fmt.Errorf("%w: a cdrom requires provision.fromImage", errDiskUnsupported)
		}

		return nil
	case hypervisorhelpers.VirtualMachineDiskTypeDisk.String():
		if !disk.Provision.Blank {
			// Copying or backing a disk from a content library image is not implemented yet; see
			// the follow-up to siderolabs/talos#14511.
			return fmt.Errorf("%w: a disk is only provisioned from provision.blank today", errDiskUnsupported)
		}

		return checkBlankDiskSupported(disk)
	default:
		return fmt.Errorf("%w: unsupported type %q", errDiskUnsupported, disk.Type)
	}
}

// checkBlankDiskSupported reports whether a blank disk describes a volume that can be made.
func checkBlankDiskSupported(disk hypervisor.VirtualMachineDiskSpec) error {
	if disk.Size == 0 {
		return fmt.Errorf("%w: a blank disk requires a size", errDiskUnsupported)
	}

	if !slices.Contains(blankDiskFormats, disk.Format) {
		return fmt.Errorf("%w: unsupported format %q, expected one of %v",
			errDiskUnsupported, disk.Format, blankDiskFormats)
	}

	if err := storagehelpers.ValidateStoragePoolName(disk.Pool); err != nil {
		return fmt.Errorf("%w: %w", errDiskUnsupported, err)
	}

	return nil
}

// blankVolumeName is what a blank disk's volume is called within its pool.
//
// Two underscores separate the two names rather than one hyphen: both are validated against
// ^[A-Za-z0-9-]+$, so a single hyphen is not injective -- "a-b" plus "c" and "a" plus "b-c" would
// name one file, and two virtual machines would share one writable volume. The separator has to be
// a character neither name can contain, which is the same reasoning that puts a slash in a disk
// status ID. At most 63 + 2 + 63 + 1 + 5 characters, well inside NAME_MAX.
func blankVolumeName(virtualMachine string, disk hypervisor.VirtualMachineDiskSpec) (string, error) {
	if err := hypervisorhelpers.ValidateName(virtualMachine); err != nil {
		return "", fmt.Errorf("%w: virtual machine %w", errDiskUnsupported, err)
	}

	if err := hypervisorhelpers.ValidateName(disk.Name); err != nil {
		return "", fmt.Errorf("%w: disk %w", errDiskUnsupported, err)
	}

	return virtualMachine + "__" + disk.Name + "." + disk.Format, nil
}

// resolveBlankDisk asks for the disk's volume and reports what came back.
//
// The volume is made by the storage slice: this authors the request and reads the answer, so the
// only writer of a pool's contents stays the controller which owns the storage daemon.
func (ctrl *VirtualMachineDiskController) resolveBlankDisk(
	ctx context.Context,
	r controller.ReaderWriter,
	name string,
	disk hypervisor.VirtualMachineDiskSpec,
	volumes map[resource.ID]struct{},
) (hypervisor.VirtualMachineDiskStatusSpec, error) {
	volumeName, err := blankVolumeName(name, disk)
	if err != nil {
		return hypervisor.VirtualMachineDiskStatusSpec{}, err
	}

	id := storage.StoragePoolVolumeID(disk.Pool, volumeName)
	volumes[id] = struct{}{}

	if err = safe.WriterModify(ctx, r,
		storage.NewStoragePoolVolumeSpec(storage.NamespaceName, id),
		func(spec *storage.StoragePoolVolumeSpec) error {
			*spec.TypedSpec() = storage.StoragePoolVolumeSpecSpec{
				Pool:     disk.Pool,
				Name:     volumeName,
				Capacity: disk.Size,
				Format:   disk.Format,
			}

			return nil
		},
	); err != nil {
		return hypervisor.VirtualMachineDiskStatusSpec{}, fmt.Errorf("failed to ask for storage pool volume %q: %w", id, err)
	}

	volumeStatus, err := safe.ReaderGetByID[*storage.StoragePoolVolumeStatus](ctx, r, id)

	switch {
	case state.IsNotFoundError(err):
		return hypervisor.VirtualMachineDiskStatusSpec{}, fmt.Errorf("waiting for volume %q in storage pool %q", volumeName, disk.Pool)
	case err != nil:
		return hypervisor.VirtualMachineDiskStatusSpec{}, fmt.Errorf("failed to get storage pool volume status %q: %w", id, err)
	case !volumeStatus.TypedSpec().Ready:
		return hypervisor.VirtualMachineDiskStatusSpec{}, fmt.Errorf("volume %q in storage pool %q is not ready: %s",
			volumeName, disk.Pool, volumeStatus.TypedSpec().Error)
	}

	return hypervisor.VirtualMachineDiskStatusSpec{
		SourcePath: volumeStatus.TypedSpec().Path,
		Format:     volumeStatus.TypedSpec().Format,
		// A blank disk is the guest's to write into; that is the whole point of it.
		ReadOnly: false,
		Ready:    true,
		Size:     volumeStatus.TypedSpec().Capacity,
	}, nil
}

// resolveVirtualMachineDisk finds the host source for a disk within a library already held for it.
func resolveVirtualMachineDisk(
	disk hypervisor.VirtualMachineDiskSpec,
	library hypervisor.ContentLibraryStatusSpec,
) (hypervisor.VirtualMachineDiskStatusSpec, error) {
	image := disk.Provision.FromImage

	if !library.Ready {
		return hypervisor.VirtualMachineDiskStatusSpec{}, fmt.Errorf("content library %q is not ready: %s", image.Library, library.Error)
	}

	if err := checkLibraryFile(library.Path, image.File, image.Digest); err != nil {
		return hypervisor.VirtualMachineDiskStatusSpec{}, fmt.Errorf("content library %q: file %q: %w", image.Library, image.File, err)
	}

	return hypervisor.VirtualMachineDiskStatusSpec{
		// Attached where it lies: nothing copies the image, so the library's mount has to stay under it.
		SourcePath: filepath.Join(library.Path, image.File),
		Format:     rawDiskFormat,
		ReadOnly:   true,
		Ready:      true,
	}, nil
}

// checkLibraryFile confirms the image exists and, when a digest is pinned, that it still hashes to it.
func checkLibraryFile(libraryPath, name, expected string) error {
	root, err := os.OpenRoot(libraryPath)
	if err != nil {
		return fmt.Errorf("failed to open content library directory: %w", err)
	}

	defer root.Close() //nolint:errcheck

	f, err := root.Open(name)
	if err != nil {
		return err
	}

	defer f.Close() //nolint:errcheck

	info, err := f.Stat()
	if err != nil {
		return err
	}

	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}

	if expected == "" {
		return nil
	}

	return verifyDigest(f, expected)
}

// verifyDigest rehashes the whole file. It runs on every reconciliation: a library file is not
// immutable, and nothing else notices when it changes underneath a virtual machine.
func verifyDigest(r io.Reader, expected string) error {
	// Machine configuration validation already accepted this digest, including its algorithm.
	dgst, err := digest.Parse(expected)
	if err != nil {
		return fmt.Errorf("digest %q is invalid: %w", expected, err)
	}

	verifier := dgst.Verifier()

	if _, err := io.Copy(verifier, r); err != nil {
		return fmt.Errorf("failed to read for digest verification: %w", err)
	}

	if !verifier.Verified() {
		return fmt.Errorf("digest mismatch: expected %s", dgst)
	}

	return nil
}
