// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package volumes

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/google/cel-go/cel"
	"github.com/siderolabs/gen/value"
	"github.com/siderolabs/gen/xerrors"
	"github.com/siderolabs/go-blockdevice/v2/partitioning"
	"go.uber.org/zap"

	blockpb "github.com/siderolabs/talos/pkg/machinery/api/resource/definitions/block"
	taloscel "github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

// LocateAndProvision locates and provisions a volume.
func LocateAndProvision(ctx context.Context, logger *zap.Logger, vc ManagerContext) error {
	// 1. Setup common status fields
	vc.Status.MountSpec = vc.Cfg.TypedSpec().Mount
	vc.Status.SymlinkSpec = vc.Cfg.TypedSpec().Symlink

	// 2. Handle simple types (Tmpfs, Overlay, External, etc.)
	// If handled, we return early.
	if done := handleSimpleVolumeTypes(vc); done {
		return nil
	}

	// 3. Validation for Disk/Partition types
	if value.IsZero(vc.Cfg.TypedSpec().Locator) {
		return fmt.Errorf("volume locator is not set")
	}

	// 4. Attempt to locate an existing volume
	located, err := locateExistingVolume(vc)
	if err != nil {
		return err
	}

	if located {
		return nil
	}

	// 5. Handle Waiting State
	// If not found and devices aren't ready, we must wait.
	if !vc.DevicesReady {
		vc.Status.Phase = block.VolumePhaseWaiting

		return nil
	}

	// 6. Provision new volume
	return provisionNewVolume(ctx, logger, vc)
}

// handleSimpleVolumeTypes handles non-provisionable types.
// Returns true if the volume type was handled.
func handleSimpleVolumeTypes(vc ManagerContext) bool {
	spec := vc.Cfg.TypedSpec()

	switch spec.Type {
	case block.VolumeTypeTmpfs, block.VolumeTypeDirectory, block.VolumeTypeSymlink, block.VolumeTypeOverlay:
		vc.Status.Phase = block.VolumePhaseReady

		return true

	case block.VolumeTypeExternal:
		vc.Status.Phase = block.VolumePhaseReady
		vc.Status.Filesystem = spec.Provisioning.FilesystemSpec.Type
		vc.Status.Location = spec.Provisioning.DiskSelector.External
		vc.Status.MountLocation = spec.Provisioning.DiskSelector.External

		return true

	case block.VolumeTypeDisk, block.VolumeTypePartition:
		fallthrough

	default:
		return false
	}
}

// locateExistingVolume iterates through discovered volumes to find a match.
func locateExistingVolume(vc ManagerContext) (bool, error) {
	var (
		matchedVol      *blockpb.DiscoveredVolumeSpec
		matchedDiskDevs = map[string]struct{}{}
		spec            = vc.Cfg.TypedSpec()
		isDiskType      = spec.Type == block.VolumeTypeDisk
	)

	// Iterate over all discovered volumes to find a match
	for _, dv := range vc.DiscoveredVolumes {
		matches, err := matchVolume(vc, dv)
		if err != nil {
			return false, err
		}

		if matches {
			// Specific check for Disk Types: ensure we don't match multiple physical disks
			if isDiskType && !spec.Locator.DiskMatch.IsZero() {
				diskDev := dv.DevPath
				if dv.ParentDevPath != "" {
					diskDev = dv.ParentDevPath
				}

				matchedDiskDevs[diskDev] = struct{}{}

				// Prefer the whole-disk discovered volume (no parent) over partition entries,
				// so that disk volumes locate the disk device path, not a partition path.
				if matchedVol == nil || (matchedVol.ParentDevPath != "" && dv.ParentDevPath == "") {
					matchedVol = dv
				}

				continue
			}

			// For Partition types, we stop at the first match
			applyLocatedStatus(vc, dv)

			return true, nil
		}
	}

	// Post-loop check for Disk Types
	if len(matchedDiskDevs) > 1 {
		disks := slices.Sorted(maps.Keys(matchedDiskDevs))

		return false, fmt.Errorf("multiple disks matched selector for disk volume; matched disks: %v", disks)
	}

	if matchedVol != nil {
		applyLocatedStatus(vc, matchedVol)

		return true, nil
	}

	return false, nil
}

// matchVolume encapsulates the CEL evaluation logic for a single discovered volume.
func matchVolume(vc ManagerContext, dv *blockpb.DiscoveredVolumeSpec) (bool, error) {
	var (
		env          *cel.Env
		expr         taloscel.Expression
		matchContext = map[string]any{}
		spec         = vc.Cfg.TypedSpec()
	)

	// Determine which locator expression to use
	switch {
	case !spec.Locator.Match.IsZero():
		env = celenv.VolumeLocator()
		expr = spec.Locator.Match
		matchContext["volume"] = dv
	case !spec.Locator.DiskMatch.IsZero():
		env = celenv.DiskLocator()
		expr = spec.Locator.DiskMatch
	default:
		return false, fmt.Errorf("no locator expression set for volume")
	}

	// Resolve the parent disk for CEL context
	for _, diskCtx := range vc.Disks {
		// Match via ParentDevPath (partition) or DevPath (disk)
		if (dv.ParentDevPath != "" && diskCtx.Disk.DevPath == dv.ParentDevPath) ||
			(dv.ParentDevPath == "" && diskCtx.Disk.DevPath == dv.DevPath) {
			matchContext["disk"] = diskCtx.Disk

			break
		}
	}

	matches, err := expr.EvalBool(env, matchContext)
	if err != nil {
		return false, fmt.Errorf("error evaluating volume locator: %w", err)
	}

	return matches, nil
}

// applyLocatedStatus updates the status when a volume is found.
func applyLocatedStatus(vc ManagerContext, vol *blockpb.DiscoveredVolumeSpec) {
	vc.Status.Phase = block.VolumePhaseLocated
	vc.Status.Location = vol.DevPath
	vc.Status.PartitionIndex = int(vol.PartitionIndex)
	vc.Status.ParentLocation = vol.ParentDevPath
	vc.Status.UUID = vol.Uuid
	vc.Status.PartitionUUID = vol.PartitionUuid
	vc.Status.SetSize(vol.Size)
}

// provisionNewVolume handles the creation/provisioning of missing volumes.
func provisionNewVolume(ctx context.Context, logger *zap.Logger, vc ManagerContext) error {
	spec := vc.Cfg.TypedSpec()

	// Pre-checks
	if value.IsZero(spec.Provisioning) {
		vc.Status.Phase = block.VolumePhaseMissing

		return nil
	}

	if !vc.PreviousWaveProvisioned {
		vc.Status.Phase = block.VolumePhaseWaiting

		return nil
	}

	// 1. Find candidate disks
	candidates, err := findCandidateDisks(vc)
	if err != nil {
		return err
	}

	logger.Debug("matched disks", zap.Strings("disks", candidates))

	// 2. Select the best fit
	pickedDisk, diskRes, err := selectBestDisk(logger, candidates, vc.Cfg)
	if err != nil {
		return err
	}

	logger.Debug("picked disk", zap.String("disk", pickedDisk))

	// 3. Apply Provisioning (Update status or Create Partition)
	return applyProvisioning(ctx, logger, vc, pickedDisk, diskRes)
}

// findCandidateDisks filters available disks based on the selector.
func findCandidateDisks(vc ManagerContext) ([]string, error) {
	var matchedDisks []string

	spec := vc.Cfg.TypedSpec()

	for _, diskCtx := range vc.Disks {
		if diskCtx.Disk.Readonly {
			continue
		}

		matches, err := spec.Provisioning.DiskSelector.Match.EvalBool(celenv.DiskLocator(), diskCtx.ToCELContext())
		if err != nil {
			return nil, fmt.Errorf("error evaluating disk locator: %w", err)
		}

		if matches {
			matchedDisks = append(matchedDisks, diskCtx.Disk.DevPath)
		}
	}

	if len(matchedDisks) == 0 {
		return nil, fmt.Errorf("no disks matched selector for volume")
	}

	if spec.Type == block.VolumeTypeDisk && len(matchedDisks) > 1 {
		return nil, fmt.Errorf("multiple disks matched selector for disk volume; matched disks: %v", matchedDisks)
	}

	return matchedDisks, nil
}

// selectBestDisk analyzes candidates and picks the one that satisfies constraints.
func selectBestDisk(logger *zap.Logger, candidates []string, cfg *block.VolumeConfig) (string, CheckDiskResult, error) {
	var (
		pickedDisk      string
		finalResult     CheckDiskResult
		rejectedReasons = map[DiskRejectedReason]int{}
	)

	for _, disk := range candidates {
		res := CheckDiskForProvisioning(logger, disk, cfg)
		if res.CanProvision {
			pickedDisk = disk
			finalResult = res

			break
		}

		rejectedReasons[res.RejectedReason]++
	}

	if pickedDisk == "" {
		err := xerrors.NewTaggedf[Retryable](
			"no disks matched for volume (%d matched selector): %d have not enough space, %d have wrong format, %d have other issues",
			len(candidates),
			rejectedReasons[NotEnoughSpace],
			rejectedReasons[WrongFormat],
			rejectedReasons[GeneralError],
		)

		return "", CheckDiskResult{}, err
	}

	return pickedDisk, finalResult, nil
}

// applyProvisioning performs the final provisioning step.
func applyProvisioning(ctx context.Context, logger *zap.Logger, vc ManagerContext, disk string, res CheckDiskResult) error {
	switch vc.Cfg.TypedSpec().Type {
	case block.VolumeTypeDisk:
		vc.Status.Phase = block.VolumePhaseProvisioned
		vc.Status.Location = disk
		vc.Status.ParentLocation = ""
		vc.Status.SetSize(res.DiskSize)

	case block.VolumeTypePartition:
		partRes, err := CreatePartition(ctx, logger, disk, vc.Cfg, res.HasGPT)
		if err != nil {
			return fmt.Errorf("error creating partition: %w", err)
		}

		vc.Status.Phase = block.VolumePhaseProvisioned
		vc.Status.Location = partitioning.DevName(disk, uint(partRes.PartitionIdx))
		vc.Status.PartitionIndex = partRes.PartitionIdx
		vc.Status.ParentLocation = disk
		vc.Status.PartitionUUID = partRes.Partition.PartGUID.String()
		vc.Status.SetSize(partRes.Size)

	case block.VolumeTypeTmpfs, block.VolumeTypeDirectory, block.VolumeTypeSymlink, block.VolumeTypeOverlay, block.VolumeTypeExternal:
		fallthrough

	default:
		panic(fmt.Sprintf("unexpected volume type: %s", vc.Cfg.TypedSpec().Type))
	}

	return nil
}
