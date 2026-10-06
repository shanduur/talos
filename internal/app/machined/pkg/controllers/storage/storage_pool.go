// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/google/uuid"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/internal/cleanup"
	machineruntime "github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/internal/pkg/libvirt"
	libvirtstorage "github.com/siderolabs/talos/internal/pkg/libvirt/storage"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"
	"github.com/siderolabs/talos/pkg/machinery/resources/storage"
)

// errPoolPending marks conditions resolved by an input event, not by restart backoff.
var errPoolPending = errors.New("pool pending")

// StoragePoolController defines libvirt directory pools on existing volume mounts.
//
// Like other mount consumers, it holds a finalizer on the VolumeMountStatus while
// the pool uses it and releases the hold once the pool is stopped.
type StoragePoolController struct {
	V1Alpha1Mode machineruntime.Mode

	// Open is injectable for deterministic reconciliation tests.
	Open func(context.Context) (libvirtstorage.Client, error)
}

// Name implements controller.Controller interface.
func (ctrl *StoragePoolController) Name() string {
	return "storage.StoragePoolController"
}

// Inputs implements controller.Controller interface.
func (ctrl *StoragePoolController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: storage.NamespaceName,
			Type:      storage.StoragePoolSpecType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: hardware.NamespaceName,
			Type:      hardware.SystemInformationType,
			ID:        optional.Some(hardware.SystemInformationID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.VolumeMountStatusType,
			Kind:      controller.InputStrong,
		},
		{
			Namespace: storage.NamespaceName,
			Type:      storage.StoragePoolStatusType,
			// Own output: nothing else emits a resource event when a consumer gives its hold back,
			// and until one does the pool and its mount stay pinned.
			Kind: controller.InputDestroyReady,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *StoragePoolController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: storage.StoragePoolStatusType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
//
//nolint:gocyclo
func (ctrl *StoragePoolController) Run(ctx context.Context, r controller.Runtime, _ *zap.Logger) error {
	if ctrl.V1Alpha1Mode.InContainer() {
		return nil
	}

	if ctrl.Open == nil {
		ctrl.Open = func(ctx context.Context) (libvirtstorage.Client, error) {
			return libvirt.New().Storage(ctx)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		specs, err := safe.ReaderListAll[*storage.StoragePoolSpec](ctx, r)
		if err != nil {
			return fmt.Errorf("error listing storage pool specs: %w", err)
		}

		mounts, err := safe.ReaderListAll[*block.VolumeMountStatus](ctx, r)
		if err != nil {
			return fmt.Errorf("error listing volume mount statuses: %w", err)
		}

		machineUUID, err := poolMachineUUID(ctx, r)
		if err != nil {
			return err
		}

		client, openErr := ctrl.Open(ctx)
		if openErr != nil {
			// The daemon stops before volumes are finalized on shutdown, so a tearing-down mount
			// can be released right away rather than blocking until its deadline -- except under a
			// held pool, where a guest holds its disk open whether or not the daemon is up.
			//
			// Kept apart from openErr: this reported the wrong reason when it shared one variable,
			// and the daemon's own error is the only thing that says why it could not be reached.
			if err := ctrl.releaseTearingDown(ctx, r, mounts); err != nil {
				return err
			}

			if specs.Len() == 0 {
				// Nothing to do without a daemon; don't restart-loop while unused.
				continue
			}

			// Daemon recovery emits no resource event: report and retry via backoff.
			if err := ctrl.reportUnavailable(ctx, r, specs, fmt.Errorf("waiting for storage daemon: %w", openErr)); err != nil {
				return err
			}

			return fmt.Errorf("waiting for storage daemon: %w", openErr)
		}

		err = ctrl.reconcile(ctx, r, client, machineUUID, specs, mounts)

		client.Close()

		if err != nil {
			return err
		}
	}
}

//nolint:gocyclo,cyclop
func (ctrl *StoragePoolController) reconcile(ctx context.Context, r controller.Runtime, client libvirtstorage.Client,
	machineUUID uuid.UUID, specs safe.List[*storage.StoragePoolSpec], mounts safe.List[*block.VolumeMountStatus],
) error {
	desired := map[string]string{}

	for spec := range specs.All() {
		if spec.Metadata().Phase() == resource.PhaseRunning {
			desired[spec.Metadata().ID()] = spec.TypedSpec().VolumeID
		}
	}

	// Remove owned definitions which are no longer desired, including ones left
	// behind by a previous boot. Foreign pools are never touched.
	pools, err := client.Pools()
	if err != nil {
		return fmt.Errorf("error listing storage pools: %w", err)
	}

	// A pool whose status something still holds is one a guest may have a volume of open. Its
	// definition and its mount both stay until the hold comes back, which the teardown below asks
	// for.
	held, err := heldPools(ctx, r)
	if err != nil {
		return err
	}

	for _, pool := range pools {
		_, wanted := desired[pool.Name]
		if wanted || pool.UUID != libvirtstorage.UUID(machineUUID, pool.Name) {
			continue
		}

		if _, isHeld := held[pool.Name]; isHeld {
			continue
		}

		if err = client.Remove(pool); err != nil {
			return fmt.Errorf("error removing storage pool %q: %w", pool.Name, err)
		}
	}

	// Release holds on mounts which are no longer usable or no longer referenced.
	// Pools defined on such a mount are stopped first so libvirt never uses a
	// mount we do not hold; activation below restarts the desired ones.
	for mount := range mounts.All() {
		if !mount.Metadata().Finalizers().Has(ctrl.Name()) {
			continue
		}

		if slices.Contains(slices.Collect(maps.Values(desired)), mount.TypedSpec().VolumeID) && mountWritable(mount) {
			continue
		}

		// Match on the libvirt definition's target, not the spec: a retargeted pool
		// still points at the old mount until Ensure redefines it.
		if poolsHeldUnder(pools, held, machineUUID, mount.TypedSpec().Target) {
			// Releasing now would unmount the filesystem a guest is writing into. Stopping the pool
			// would not: a directory pool is metadata, and a domain opens its disks by absolute
			// path. The mount is the part that cannot be taken away, so neither happens until the
			// hold does come back.
			continue
		}

		for _, pool := range pools {
			if pool.UUID != libvirtstorage.UUID(machineUUID, pool.Name) || !pathUnder(pool.Target, mount.TypedSpec().Target) {
				continue
			}

			if err = client.Stop(pool); err != nil {
				return fmt.Errorf("error stopping storage pool %q: %w", pool.Name, err)
			}
		}

		if err = r.RemoveFinalizer(ctx, mount.Metadata(), ctrl.Name()); err != nil {
			return fmt.Errorf("error removing finalizer from volume mount status %q: %w", mount.Metadata().ID(), err)
		}
	}

	var activateErrors error

	for name, volumeID := range desired {
		pool := libvirtstorage.Pool{Name: name, UUID: libvirtstorage.UUID(machineUUID, name)}

		target, activateErr := ctrl.activate(ctx, r, client, pool, volumeID)
		if activateErr != nil && !errors.Is(activateErr, errPoolPending) {
			// libvirt/filesystem failures emit no resource event: retry via backoff.
			activateErrors = errors.Join(activateErrors, fmt.Errorf("pool %q: %w", name, activateErr))
		}

		if err = safe.WriterModify(ctx, r, storage.NewStoragePoolStatus(storage.NamespaceName, name), func(status *storage.StoragePoolStatus) error {
			status.TypedSpec().VolumeID = volumeID
			status.TypedSpec().Ready = activateErr == nil
			status.TypedSpec().Error = ""

			// Kept rather than cleared when activation fails: TargetPath is the record of where the
			// pool was last put, and releaseTearingDown matches a tearing-down mount against it to
			// decide whether a guest may still be writing there. A failed activation clearing it --
			// which a mount beginning to tear down causes -- hands that mount straight back.
			// Nothing reads it while Ready is false.
			if target != "" {
				status.TypedSpec().TargetPath = target
			}

			if activateErr != nil {
				status.TypedSpec().Error = activateErr.Error()
			}

			return nil
		}); err != nil {
			return fmt.Errorf("error updating storage pool status %q: %w", name, err)
		}
	}

	// Torn down rather than destroyed outright: a status a consumer holds cannot be destroyed, and
	// attempting it would fail this reconciliation for as long as the hold lasts.
	if err = cleanup.Outputs[*storage.StoragePoolStatus](ctx, r, "storage pool status", wantedPoolStatuses(desired)); err != nil {
		return err
	}

	return activateErrors
}

// wantedPoolStatuses names the statuses the configuration still asks for.
func wantedPoolStatuses(desired map[string]string) map[resource.ID]struct{} {
	wanted := make(map[resource.ID]struct{}, len(desired))

	for name := range desired {
		wanted[name] = struct{}{}
	}

	return wanted
}

// heldPools names the pools whose status something still holds, by pool name.
//
// A hold means a consumer -- a volume of this pool, and through it a running guest -- is still
// relying on the pool's directory being where it is.
func heldPools(ctx context.Context, r controller.Reader) (map[string]struct{}, error) {
	statuses, err := safe.ReaderListAll[*storage.StoragePoolStatus](ctx, r)
	if err != nil {
		return nil, fmt.Errorf("error listing storage pool statuses: %w", err)
	}

	held := map[string]struct{}{}

	for status := range statuses.All() {
		if !status.Metadata().Finalizers().Empty() {
			held[status.Metadata().ID()] = struct{}{}
		}
	}

	return held, nil
}

// poolsHeldUnder reports whether any held pool is defined under target.
func poolsHeldUnder(pools []libvirtstorage.Pool, held map[string]struct{}, machineUUID uuid.UUID, target string) bool {
	for _, pool := range pools {
		if pool.UUID != libvirtstorage.UUID(machineUUID, pool.Name) || !pathUnder(pool.Target, target) {
			continue
		}

		if _, isHeld := held[pool.Name]; isHeld {
			return true
		}
	}

	return false
}

// activate exposes the pool once its backing mount is held.
//
// A pending mount is reported through the status wrapped in errPoolPending, not
// retried: the mount status input wakes the controller when it changes.
func (ctrl *StoragePoolController) activate(ctx context.Context, r controller.Runtime, client libvirtstorage.Client,
	pool libvirtstorage.Pool, volumeID string,
) (string, error) {
	mount, err := safe.ReaderGetByID[*block.VolumeMountStatus](ctx, r, volumeID)
	if err != nil {
		if state.IsNotFoundError(err) {
			return "", fmt.Errorf("waiting for backing volume %q to be mounted: %w", volumeID, errPoolPending)
		}

		return "", fmt.Errorf("error getting volume mount status: %w", err)
	}

	if !mountWritable(mount) {
		return "", fmt.Errorf("backing volume %q is not mounted for writing: %w", volumeID, errPoolPending)
	}

	if !mount.Metadata().Finalizers().Has(ctrl.Name()) {
		if err = r.AddFinalizer(ctx, mount.Metadata(), ctrl.Name()); err != nil {
			return "", fmt.Errorf("error adding finalizer to volume mount status: %w", err)
		}
	}

	target := filepath.Join(mount.TypedSpec().Target, pool.Name)

	if err = client.Ensure(pool, target, func() error { return preparePoolDirectory(target) }); err != nil {
		return "", err
	}

	return target, nil
}

// releaseTearingDown drops our hold on mounts being torn down while the daemon is unavailable.
//
// A mount under a held pool is kept, though. "No daemon means no pool is running, so nothing is
// writing" holds only for a pool whose contents are read through libvirt. A guest holds its disk
// open by file descriptor, so the storage daemon dying says nothing about whether the filesystem is
// busy -- and a daemon which merely crashed is not a shutdown.
func (ctrl *StoragePoolController) releaseTearingDown(ctx context.Context, r controller.Runtime, mounts safe.List[*block.VolumeMountStatus]) error {
	held, err := heldPools(ctx, r)
	if err != nil {
		return err
	}

	var targets map[string]struct{}

	if len(held) > 0 {
		// Without a daemon the pools cannot be enumerated, so the status's own record of where the
		// pool was put is the only thing left to match a mount against.
		if targets, err = heldPoolTargets(ctx, r, held); err != nil {
			return err
		}
	}

	for mount := range mounts.All() {
		if mount.Metadata().Phase() != resource.PhaseTearingDown || !mount.Metadata().Finalizers().Has(ctrl.Name()) {
			continue
		}

		if mountUnderAny(mount.TypedSpec().Target, targets) {
			continue
		}

		if err := r.RemoveFinalizer(ctx, mount.Metadata(), ctrl.Name()); err != nil {
			return fmt.Errorf("error removing finalizer from volume mount status %q: %w", mount.Metadata().ID(), err)
		}
	}

	return nil
}

// heldPoolTargets collects the directories of the held pools, as their statuses last reported them.
func heldPoolTargets(ctx context.Context, r controller.Reader, held map[string]struct{}) (map[string]struct{}, error) {
	statuses, err := safe.ReaderListAll[*storage.StoragePoolStatus](ctx, r)
	if err != nil {
		return nil, fmt.Errorf("error listing storage pool statuses: %w", err)
	}

	targets := map[string]struct{}{}

	for status := range statuses.All() {
		if _, isHeld := held[status.Metadata().ID()]; !isHeld {
			continue
		}

		if target := status.TypedSpec().TargetPath; target != "" {
			targets[target] = struct{}{}
		}
	}

	return targets, nil
}

// mountUnderAny reports whether any of the directories lies at or below the mount's target.
func mountUnderAny(mountTarget string, targets map[string]struct{}) bool {
	for target := range targets {
		if pathUnder(target, mountTarget) {
			return true
		}
	}

	return false
}

// reportUnavailable writes the daemon error to every desired pool status.
func (ctrl *StoragePoolController) reportUnavailable(ctx context.Context, r controller.Runtime, specs safe.List[*storage.StoragePoolSpec], reason error) error {
	wanted := map[resource.ID]struct{}{}

	for spec := range specs.All() {
		if spec.Metadata().Phase() != resource.PhaseRunning {
			continue
		}

		wanted[spec.Metadata().ID()] = struct{}{}

		if err := safe.WriterModify(ctx, r, storage.NewStoragePoolStatus(storage.NamespaceName, spec.Metadata().ID()), func(status *storage.StoragePoolStatus) error {
			// Assigned field by field rather than overwritten: TargetPath is where the pool was put,
			// and with the daemon unreachable it is the only record left of that. releaseTearingDown
			// matches a mount against it to decide whether a guest may still be writing there, so
			// clearing it hands the mount back on the very next pass.
			status.TypedSpec().VolumeID = spec.TypedSpec().VolumeID
			status.TypedSpec().Ready = false
			status.TypedSpec().Error = reason.Error()

			return nil
		}); err != nil {
			return fmt.Errorf("error updating storage pool status %q: %w", spec.Metadata().ID(), err)
		}
	}

	return cleanup.Outputs[*storage.StoragePoolStatus](ctx, r, "storage pool status", wanted)
}

// mountWritable is the single predicate for a mount a pool may be defined on.
func mountWritable(mount *block.VolumeMountStatus) bool {
	spec := mount.TypedSpec()

	return mount.Metadata().Phase() == resource.PhaseRunning && !spec.ReadOnly && !spec.Detached && filepath.IsAbs(spec.Target)
}

// pathUnder reports whether path is dir or lies below it.
func pathUnder(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}

	rel, err := filepath.Rel(dir, path)

	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func poolMachineUUID(ctx context.Context, r controller.Reader) (uuid.UUID, error) {
	system, err := safe.ReaderGetByID[*hardware.SystemInformation](ctx, r, hardware.SystemInformationID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("error getting system information: %w", err)
	}

	machineUUID, err := uuid.Parse(system.TypedSpec().UUID)
	if err != nil || machineUUID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("invalid machine UUID %q", system.TypedSpec().UUID)
	}

	return machineUUID, nil
}

func preparePoolDirectory(target string) error {
	// Never MkdirAll: it could create an absent mount target on the rootfs.
	// Refuse symlinks so a pool cannot escape its backing volume.
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return os.Mkdir(target, 0o755)
	}

	if err != nil {
		return err
	}

	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("pool target %q is not a directory", target)
	}

	return nil
}
