// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package mdd

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/siderolabs/go-blockdevice/v2/block"
	"github.com/siderolabs/go-blockdevice/v2/partitioning"
	"github.com/siderolabs/go-blockdevice/v2/partitioning/gpt"

	"github.com/siderolabs/talos/internal/pkg/partition"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/imager/quirks"
	"github.com/siderolabs/talos/pkg/machinery/version"
)

// partitionMirrorMember wipes a disk and lays down the mirror-member layout:
// an EFI System Partition (kept outside the array so UEFI can boot from this
// disk) followed by a RAID-member partition spanning the rest. It returns the
// ESP and RAID-member device paths.
//
// The ESP is left unformatted here; the installer formats it and writes the
// UKI. The RAID member carries the LinuxRAID partition type so it is never
// mistaken for a directly-usable filesystem.
func partitionMirrorMember(disk string) (espDev, memberDev string, err error) {
	bd, err := block.NewFromPath(disk, block.OpenForWrite())
	if err != nil {
		return "", "", fmt.Errorf("failed to open %s: %w", disk, err)
	}

	defer bd.Close() //nolint:errcheck

	if err = bd.Lock(true); err != nil {
		return "", "", fmt.Errorf("failed to lock %s: %w", disk, err)
	}

	defer bd.Unlock() //nolint:errcheck

	if err = bd.FastWipe(); err != nil {
		return "", "", fmt.Errorf("failed to wipe %s: %w", disk, err)
	}

	gptdev, err := gpt.DeviceFromBlockDevice(bd)
	if err != nil {
		return "", "", fmt.Errorf("failed to init GPT device from %s: %w", disk, err)
	}

	pt, err := gpt.New(gptdev)
	if err != nil {
		return "", "", fmt.Errorf("failed to init GPT on %s: %w", disk, err)
	}

	quirk := quirks.New(version.Tag)

	// ESP, sized as for a UKI install.
	esp := partition.NewPartitionOptions(true, quirk, partition.WithLabel(constants.EFIPartitionLabel))
	if _, _, err = pt.AllocatePartition(esp.Size, esp.PartitionLabel, uuid.MustParse(esp.PartitionType), esp.PartitionOpts...); err != nil {
		return "", "", fmt.Errorf("failed to allocate ESP on %s: %w", disk, err)
	}

	// RAID member spanning the remaining space.
	if _, _, err = pt.AllocatePartition(pt.LargestContiguousAllocatable(), "", uuid.MustParse(partition.LinuxRAIDPartition)); err != nil {
		return "", "", fmt.Errorf("failed to allocate RAID member on %s: %w", disk, err)
	}

	if err = pt.Write(); err != nil {
		return "", "", fmt.Errorf("failed to write GPT on %s: %w", disk, err)
	}

	if err = gptdev.Sync(); err != nil {
		return "", "", fmt.Errorf("failed to sync GPT on %s: %w", disk, err)
	}

	return partitioning.DevName(disk, 1), partitioning.DevName(disk, 2), nil
}
