// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/internal/cleanup"
	machineruntime "github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/internal/pkg/libvirt"
	libvirtstorage "github.com/siderolabs/talos/internal/pkg/libvirt/storage"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"
	"github.com/siderolabs/talos/pkg/machinery/resources/hypervisor"
	"github.com/siderolabs/talos/pkg/machinery/resources/storage"
)

const volumeControllerName = "storage.StoragePoolVolumeController"

// errVolumePending marks a condition an input event resolves, rather than restart backoff.
var errVolumePending = errors.New("volume pending")

// pendingError carries errVolumePending without saying so.
//
// The condition is reported through the volume's status, which an operator reads: prefixing it with
// the sentinel's own text would say "volume pending" of conditions which are not pending at all,
// such as a volume whose format differs from the one asked for.
type pendingError struct{ err error }

func (e pendingError) Error() string   { return e.err.Error() }
func (e pendingError) Unwrap() []error { return []error{errVolumePending, e.err} }

// pending marks a condition the status reports and an input event resolves, rather than one restart
// backoff should retry.
func pending(format string, args ...any) error { return pendingError{fmt.Errorf(format, args...)} }

// StoragePoolVolumeController makes the volumes asked for exist in their storage pools.
//
// It is the only writer of volumes, as StoragePoolController is the only writer of pools: both
// speak to the same storage daemon, and a pool being redefined underneath a volume being created is
// not something two controllers could agree on after the fact.
//
// It never deletes a volume. A spec going away means nothing is asking for the volume any more, not
// that its contents may go: the configuration which asked for a disk is not the owner of what a
// guest then wrote into it.
type StoragePoolVolumeController struct {
	V1Alpha1Mode machineruntime.Mode

	// Open is injectable for deterministic reconciliation tests.
	Open func(context.Context) (libvirtstorage.Client, error)
}

// Name implements controller.Controller interface.
func (ctrl *StoragePoolVolumeController) Name() string {
	return volumeControllerName
}

// Inputs implements controller.Controller interface.
func (ctrl *StoragePoolVolumeController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: storage.NamespaceName,
			Type:      storage.StoragePoolVolumeSpecType,
			// Strong: the spec is held while its volume is in use, so whoever authored it cannot
			// withdraw the volume out from under a guest.
			Kind: controller.InputStrong,
		},
		{
			Namespace: storage.NamespaceName,
			Type:      storage.StoragePoolStatusType,
			// Strong: a volume lives in the pool's directory, so that directory has to outlive
			// every guest reading from one.
			Kind: controller.InputStrong,
		},
		{
			Namespace: storage.NamespaceName,
			Type:      storage.StoragePoolVolumeStatusType,
			Kind:      controller.InputDestroyReady,
		},
		{
			Namespace: hypervisor.NamespaceName,
			Type:      hypervisor.VirtualMachineDiskStatusType,
			// Read to find out which volumes a guest has open. A volume one is reading cannot be
			// resized underneath it, so this is what defers a growth rather than forcing it.
			Kind: controller.InputWeak,
		},
		{
			Namespace: hardware.NamespaceName,
			Type:      hardware.SystemInformationType,
			ID:        optional.Some(hardware.SystemInformationID),
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *StoragePoolVolumeController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: storage.StoragePoolVolumeStatusType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *StoragePoolVolumeController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	if ctrl.V1Alpha1Mode.InContainer() {
		return nil
	}

	if ctrl.Open == nil {
		ctrl.Open = func(ctx context.Context) (libvirtstorage.Client, error) {
			return libvirt.New().StorageVolumes(ctx)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		specs, err := safe.ReaderListAll[*storage.StoragePoolVolumeSpec](ctx, r)
		if err != nil {
			return fmt.Errorf("error listing storage pool volume specs: %w", err)
		}

		if err = ctrl.reconcile(ctx, r, logger, specs); err != nil {
			return err
		}

		r.ResetRestartBackoff()
	}
}

// reconcile brings every asked-for volume into being, opening a session only if one is needed.
func (ctrl *StoragePoolVolumeController) reconcile(
	ctx context.Context, r controller.Runtime, logger *zap.Logger, specs safe.List[*storage.StoragePoolVolumeSpec],
) error {
	pools, err := safe.ReaderListAll[*storage.StoragePoolStatus](ctx, r)
	if err != nil {
		return fmt.Errorf("error listing storage pool statuses: %w", err)
	}

	poolStatuses := make(map[string]*storage.StoragePoolStatus, pools.Len())

	for pool := range pools.All() {
		poolStatuses[pool.Metadata().ID()] = pool
	}

	inUse, poolsInUse, err := volumesInUse(ctx, r)
	if err != nil {
		return err
	}

	// Seeded with the pools a guest already has a volume of open: those stay held whether or not a
	// spec resolves against them this pass, and in particular while one is tearing down.
	held := maps.Clone(poolsInUse)

	var (
		session libvirtstorage.Client
		errs    []error
	)

	defer func() {
		if session != nil {
			session.Close()
		}
	}()

	// Opened lazily, and at most once: a host with no volumes to reconcile must not dial the daemon
	// at all, or every pass turns a daemon outage into a restart loop for nothing.
	openSession := func() (libvirtstorage.Client, error) {
		if session != nil {
			return session, nil
		}

		opened, openErr := ctrl.Open(ctx)
		if openErr != nil {
			return nil, pending("waiting for storage daemon: %w", openErr)
		}

		session = opened

		return session, nil
	}

	wanted, specErrs, err := ctrl.reconcileSpecs(ctx, r, logger, specs, poolStatuses, inUse, held, openSession)

	errs = append(errs, specErrs...)

	if err != nil {
		return errors.Join(append(errs, err)...)
	}

	// A status nothing asks for any more is torn down rather than destroyed, so whatever holds it
	// has a chance to notice and give the hold back. The volume's file is untouched either way.
	if err = cleanup.Outputs[*storage.StoragePoolVolumeStatus](ctx, r, "storage pool volume status", wanted); err != nil {
		return errors.Join(append(errs, err)...)
	}

	if err = ctrl.releaseSpecs(ctx, r, specs, wanted); err != nil {
		return errors.Join(append(errs, err)...)
	}

	return errors.Join(append(errs, ctrl.releasePools(ctx, r, logger, poolStatuses, held))...)
}

// reconcileSpecs publishes the status of every volume still asked for, and names them.
//
// A per-volume failure is collected rather than returned: one unreachable volume must not stop the
// rest from being reported. Only a failure to write a status at all is returned.
func (ctrl *StoragePoolVolumeController) reconcileSpecs(
	ctx context.Context,
	r controller.ReaderWriter,
	logger *zap.Logger,
	specs safe.List[*storage.StoragePoolVolumeSpec],
	poolStatuses map[string]*storage.StoragePoolStatus,
	inUse, held map[string]struct{},
	openSession func() (libvirtstorage.Client, error),
) (map[resource.ID]struct{}, []error, error) {
	wanted := map[resource.ID]struct{}{}

	var errs []error

	for spec := range specs.All() {
		if spec.Metadata().Phase() != resource.PhaseRunning {
			continue
		}

		id := spec.Metadata().ID()
		wanted[id] = struct{}{}

		if err := ctrl.holdSpec(ctx, r, spec); err != nil {
			errs = append(errs, err)

			continue
		}

		resolved, resolveErr := ctrl.resolve(ctx, r, logger, spec.TypedSpec(), poolStatuses, inUse, held, openSession)
		if resolveErr != nil && !errors.Is(resolveErr, errVolumePending) {
			// A libvirt or filesystem failure emits no resource event: retry via backoff.
			errs = append(errs, fmt.Errorf("volume %q: %w", id, resolveErr))
		}

		if err := safe.WriterModify(ctx, r,
			storage.NewStoragePoolVolumeStatus(storage.NamespaceName, id),
			func(status *storage.StoragePoolVolumeStatus) error {
				*status.TypedSpec() = resolved
				status.TypedSpec().Pool = spec.TypedSpec().Pool
				status.TypedSpec().Name = spec.TypedSpec().Name

				if resolveErr != nil {
					status.TypedSpec().Error = resolveErr.Error()
				}

				return nil
			},
		); err != nil {
			return wanted, errs, fmt.Errorf("error updating storage pool volume status %q: %w", id, err)
		}
	}

	return wanted, errs, nil
}

// holdSpec takes a hold on the spec, so the volume's existence is not withdrawn before its status is.
func (ctrl *StoragePoolVolumeController) holdSpec(ctx context.Context, r controller.ReaderWriter, spec *storage.StoragePoolVolumeSpec) error {
	if spec.Metadata().Finalizers().Has(ctrl.Name()) {
		return nil
	}

	if err := r.AddFinalizer(ctx, spec.Metadata(), ctrl.Name()); err != nil {
		return fmt.Errorf("error holding storage pool volume spec %q: %w", spec.Metadata().ID(), err)
	}

	return nil
}

// releaseSpecs gives back the hold on every spec whose status is gone.
func (ctrl *StoragePoolVolumeController) releaseSpecs(
	ctx context.Context, r controller.ReaderWriter, specs safe.List[*storage.StoragePoolVolumeSpec], wanted map[resource.ID]struct{},
) error {
	for spec := range specs.All() {
		id := spec.Metadata().ID()

		if _, keep := wanted[id]; keep || !spec.Metadata().Finalizers().Has(ctrl.Name()) {
			continue
		}

		// The status is withdrawn first: releasing the spec while a status still names the volume
		// would leave nothing to report it through.
		switch _, err := safe.ReaderGetByID[*storage.StoragePoolVolumeStatus](ctx, r, id); {
		case err == nil:
			continue
		case !state.IsNotFoundError(err):
			return fmt.Errorf("error getting storage pool volume status %q: %w", id, err)
		}

		if err := r.RemoveFinalizer(ctx, spec.Metadata(), ctrl.Name()); err != nil && !state.IsNotFoundError(err) {
			return fmt.Errorf("error releasing storage pool volume spec %q: %w", id, err)
		}
	}

	return nil
}

// holdPool keeps a pool's directory in place for as long as a volume of it is asked for. One which
// is tearing down is refused rather than held, as holding it now would block that teardown forever.
func (ctrl *StoragePoolVolumeController) holdPool(
	ctx context.Context, r controller.ReaderWriter, logger *zap.Logger, pool *storage.StoragePoolStatus,
) error {
	if pool.Metadata().Phase() != resource.PhaseRunning {
		return pending("storage pool %q is going away", pool.Metadata().ID())
	}

	if pool.Metadata().Finalizers().Has(ctrl.Name()) {
		return nil
	}

	if err := r.AddFinalizer(ctx, pool.Metadata(), ctrl.Name()); err != nil {
		return fmt.Errorf("error holding storage pool %q: %w", pool.Metadata().ID(), err)
	}

	logger.Info("holding storage pool for a volume", zap.String("storage_pool", pool.Metadata().ID()))

	return nil
}

// releasePools gives back the hold on every pool nothing needs any more.
//
// held is what must stay held: the pools a spec resolved against this pass, plus the pools a running
// domain has a volume of open. The second is what keeps a pool in place for a guest which is still
// writing into it, and it cannot be read off the volume statuses -- nothing finalizes those.
func (ctrl *StoragePoolVolumeController) releasePools(
	ctx context.Context, r controller.ReaderWriter, logger *zap.Logger,
	poolStatuses map[string]*storage.StoragePoolStatus, held map[string]struct{},
) error {
	for id, pool := range poolStatuses {
		if _, used := held[id]; used || !pool.Metadata().Finalizers().Has(ctrl.Name()) {
			continue
		}

		if err := r.RemoveFinalizer(ctx, pool.Metadata(), ctrl.Name()); err != nil && !state.IsNotFoundError(err) {
			return fmt.Errorf("error releasing storage pool %q: %w", id, err)
		}

		logger.Info("released storage pool held for a volume", zap.String("storage_pool", id))
	}

	return nil
}

// volumesInUse names the volumes a guest may have open, by "<pool>/<name>", and the pools those
// volumes live in.
//
// A disk status with a finalizer is one a running domain holds, so its volume is one QEMU has open
// by file descriptor. Taken from the status rather than from the domain, because a status that
// failed to resolve still names what it was for.
func volumesInUse(ctx context.Context, r controller.Reader) (volumes, pools map[string]struct{}, err error) {
	diskStatuses, err := safe.ReaderListAll[*hypervisor.VirtualMachineDiskStatus](ctx, r)
	if err != nil {
		return nil, nil, fmt.Errorf("error listing virtual machine disk statuses: %w", err)
	}

	volumes = map[string]struct{}{}
	pools = map[string]struct{}{}

	for diskStatus := range diskStatuses.All() {
		if diskStatus.Metadata().Finalizers().Empty() {
			continue
		}

		spec := diskStatus.TypedSpec()

		if spec.Pool != "" && spec.Volume != "" {
			volumes[storage.StoragePoolVolumeID(spec.Pool, spec.Volume)] = struct{}{}
			pools[spec.Pool] = struct{}{}
		}
	}

	return volumes, pools, nil
}

// resolve makes one volume be what the spec asks for, as far as it safely can.
//
// What it will not do is as much the point as what it will: it never deletes, never shrinks, never
// rewrites a volume whose format differs from the one asked for, and never resizes one a guest has
// open. Each of those is reported rather than forced.
func (ctrl *StoragePoolVolumeController) resolve(
	ctx context.Context,
	r controller.ReaderWriter,
	logger *zap.Logger,
	spec *storage.StoragePoolVolumeSpecSpec,
	poolStatuses map[string]*storage.StoragePoolStatus,
	inUse map[string]struct{},
	held map[string]struct{},
	openSession func() (libvirtstorage.Client, error),
) (storage.StoragePoolVolumeStatusSpec, error) {
	pool, err := ctrl.resolvePool(ctx, r, logger, spec.Pool, poolStatuses, held)
	if err != nil {
		return storage.StoragePoolVolumeStatusSpec{}, err
	}

	session, err := openSession()
	if err != nil {
		return storage.StoragePoolVolumeStatusSpec{}, err
	}

	volume, exists, err := session.Volume(pool, spec.Name)
	if err != nil {
		return storage.StoragePoolVolumeStatusSpec{}, err
	}

	if !exists {
		if volume, err = session.CreateVolume(pool, spec.Name, spec.Format, spec.Capacity); err != nil {
			return storage.StoragePoolVolumeStatusSpec{}, err
		}

		logger.Info("created storage pool volume",
			zap.String("storage_pool", spec.Pool), zap.String("volume", spec.Name),
			zap.String("format", spec.Format), zap.Uint64("capacity", spec.Capacity))

		return readyVolume(volume), nil
	}

	return ctrl.adopt(session, logger, pool, spec, volume, inUse)
}

// resolvePool holds the pool a volume lives in and names it to libvirt, or says why it cannot yet.
func (ctrl *StoragePoolVolumeController) resolvePool(
	ctx context.Context,
	r controller.ReaderWriter,
	logger *zap.Logger,
	name string,
	poolStatuses map[string]*storage.StoragePoolStatus,
	held map[string]struct{},
) (libvirtstorage.Pool, error) {
	poolStatus, found := poolStatuses[name]
	if !found {
		return libvirtstorage.Pool{}, pending(
			"storage pool %q has no status yet: it is either not declared by a StoragePool document, or not reconciled",
			name)
	}

	if err := ctrl.holdPool(ctx, r, logger, poolStatus); err != nil {
		return libvirtstorage.Pool{}, err
	}

	held[name] = struct{}{}

	if !poolStatus.TypedSpec().Ready {
		return libvirtstorage.Pool{}, pending("storage pool %q is not ready: %s", name, poolStatus.TypedSpec().Error)
	}

	machineUUID, err := poolMachineUUID(ctx, r)
	if err != nil {
		// Reported rather than retried: at boot the machine's own identity may simply not have been
		// read yet, and the SystemInformation input is what says when it has.
		return libvirtstorage.Pool{}, pending("%w", err)
	}

	return libvirtstorage.Pool{Name: name, UUID: libvirtstorage.UUID(machineUUID, name)}, nil
}

// adopt takes a volume already in the pool as the one the spec means, growing it if it may.
//
// That is not a fallback, it is the ordinary case: after a reboot the volume is always already
// there, and a configuration naming the same virtual machine, disk and pool is saying it means the
// same disk.
func (ctrl *StoragePoolVolumeController) adopt(
	session libvirtstorage.Client,
	logger *zap.Logger,
	pool libvirtstorage.Pool,
	spec *storage.StoragePoolVolumeSpecSpec,
	volume libvirtstorage.Volume,
	inUse map[string]struct{},
) (storage.StoragePoolVolumeStatusSpec, error) {
	if volume.Format != spec.Format {
		// Never rewritten: attaching a qcow2 file as raw hands the guest its header as block zero,
		// and the guest writes over it.
		return storage.StoragePoolVolumeStatusSpec{Capacity: volume.Capacity, Format: volume.Format, Path: volume.Path},
			pending("volume %q in storage pool %q is %s, but %s was asked for: change the format back, or move the file aside",
				spec.Name, spec.Pool, volume.Format, spec.Format)
	}

	switch {
	case volume.Capacity >= spec.Capacity:
		status := readyVolume(volume)

		if volume.Capacity > spec.Capacity {
			// Reported, not applied, and not a reason to refuse the disk: a volume larger than
			// asked for is perfectly attachable, and stopping a running guest over an edited number
			// is a worse outcome than the mismatch.
			return status, pending("volume %q in storage pool %q is %d bytes, larger than the %d asked for: a volume is never shrunk",
				spec.Name, spec.Pool, volume.Capacity, spec.Capacity)
		}

		return status, nil
	case ctrl.volumeIsOpen(spec, inUse):
		// Growing a volume a guest has open is not safe: for qcow2 QEMU holds a write lock and the
		// resize fails, and for raw it succeeds while the guest goes on seeing the old size. The
		// growth is remembered and applied once the guest stops.
		status := readyVolume(volume)
		status.PendingCapacity = spec.Capacity

		return status, pending("volume %q in storage pool %q cannot be grown to %d bytes while a virtual machine has it open: stop the virtual machine to apply it",
			spec.Name, spec.Pool, spec.Capacity)
	}

	if err := session.ResizeVolume(pool, spec.Name, spec.Capacity); err != nil {
		return storage.StoragePoolVolumeStatusSpec{}, err
	}

	logger.Info("grew storage pool volume",
		zap.String("storage_pool", spec.Pool), zap.String("volume", spec.Name),
		zap.Uint64("from", volume.Capacity), zap.Uint64("to", spec.Capacity))

	// Not looked up again: the resize passed no flags, so libvirt set the capacity outright, and
	// neither the path nor the format is a thing a resize changes.
	volume.Capacity = spec.Capacity

	return readyVolume(volume), nil
}

// volumeIsOpen reports whether a running domain holds the disk this volume backs.
func (ctrl *StoragePoolVolumeController) volumeIsOpen(spec *storage.StoragePoolVolumeSpecSpec, inUse map[string]struct{}) bool {
	_, open := inUse[storage.StoragePoolVolumeID(spec.Pool, spec.Name)]

	return open
}

// readyVolume reports a volume which may be attached.
func readyVolume(volume libvirtstorage.Volume) storage.StoragePoolVolumeStatusSpec {
	return storage.StoragePoolVolumeStatusSpec{
		Path:     volume.Path,
		Format:   volume.Format,
		Capacity: volume.Capacity,
		Ready:    true,
	}
}
