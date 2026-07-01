// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/siderolabs/gen/xslices"
	"github.com/siderolabs/go-blockdevice/v2/blkid"

	bootloaderpkg "github.com/siderolabs/talos/internal/app/machined/pkg/runtime/v1alpha1/bootloader"
	"github.com/siderolabs/talos/internal/pkg/partition"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

const sysBlockDir = "/sys/block"

// isMDDevice reports whether the given device path is an assembled MD (software
// RAID) array, by resolving it to a kernel node and checking sysfs.
//
// The install target for a mirrored system disk is a pre-created array (built
// by `talosctl md create`), e.g. /dev/disk/by-id/md-name-talos -> /dev/md0.
func isMDDevice(devPath string) bool {
	resolved, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return false
	}

	// /sys/block/<mdN>/md exists only for md arrays.
	_, err = os.Stat(filepath.Join(sysBlockDir, filepath.Base(resolved), "md"))

	return err == nil
}

// mdMemberDisks returns the parent disks of an assembled array's members, in
// sysfs order (e.g. ["/dev/vda", "/dev/vdb"] for an array over vda2+vdb2).
//
// Members are read from /sys/block/<mdN>/slaves; each slave is a member
// partition whose parent disk carries the ESP that must receive the bootloader.
func mdMemberDisks(arrayDev string) ([]string, error) {
	resolved, err := filepath.EvalSymlinks(arrayDev)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve %s: %w", arrayDev, err)
	}

	slavesDir := filepath.Join(sysBlockDir, filepath.Base(resolved), "slaves")

	entries, err := os.ReadDir(slavesDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", slavesDir, err)
	}

	var (
		disks []string
		seen  = map[string]struct{}{}
	)

	for _, entry := range entries {
		// entry is a member partition (e.g. "vda2"); resolve to its sysfs dir
		// and take the parent directory's name as the whole disk (e.g. "vda").
		target, err := filepath.EvalSymlinks(filepath.Join(slavesDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("failed to resolve member %s: %w", entry.Name(), err)
		}

		disk := filepath.Base(filepath.Dir(target))

		if _, ok := seen[disk]; ok {
			continue
		}

		seen[disk] = struct{}{}

		disks = append(disks, filepath.Join("/dev", disk))
	}

	if len(disks) == 0 {
		return nil, fmt.Errorf("no member disks found for array %s", arrayDev)
	}

	return disks, nil
}

// installMirrorBootloader formats and installs the bootloader onto every member
// disk's ESP. The ESPs were created (unformatted) by `talosctl md create`; here
// each is formatted vFAT, populated with the UKI, and registered as a UEFI boot
// entry, so the node boots from whichever disk survives.
//
// It temporarily repoints i.options.DiskPath at each member so the shared
// formatPartitions/installBootloader helpers operate on it; DiskPath is
// restored before returning (the caller still needs it pointing at the array).
func (i *Installer) installMirrorBootloader(ctx context.Context, mode Mode, bootloader bootloaderpkg.Bootloader, bootPartitions []partition.Options) error {
	members, err := mdMemberDisks(i.options.DiskPath)
	if err != nil {
		return err
	}

	// keep only the ESP boot partition(s): the member disks carry just the ESP
	// (everything else is the RAID member), so formatPartitions lines up p1=ESP.
	espPartitions := filterESPPartitions(bootPartitions)
	if len(espPartitions) == 0 {
		return fmt.Errorf("no EFI partition in boot layout; mirror install requires a UKI/sd-boot setup")
	}

	origDiskPath := i.options.DiskPath
	defer func() { i.options.DiskPath = origDiskPath }()

	for _, member := range members {
		i.options.DiskPath = member

		if err := i.formatPartitions(ctx, mode, espPartitions); err != nil {
			return fmt.Errorf("failed to format ESP on %s: %w", member, err)
		}

		info, err := blkid.ProbePath(member, blkid.WithSkipLocking(true))
		if err != nil {
			return fmt.Errorf("failed to probe %s: %w", member, err)
		}

		if _, err := i.installBootloader(ctx, mode, bootloader, info); err != nil {
			return fmt.Errorf("failed to install bootloader on %s: %w", member, err)
		}
	}

	return nil
}

// filterESPPartitions returns the EFI partition option(s) from a boot layout.
// A mirrored system disk only carries the ESP on each member (BIOS/BOOT GRUB
// partitions are not supported for the mirror layout, which is UKI/sd-boot).
func filterESPPartitions(bootPartitions []partition.Options) []partition.Options {
	return xslices.Filter(bootPartitions, func(p partition.Options) bool {
		return p.PartitionLabel == constants.EFIPartitionLabel
	})
}
