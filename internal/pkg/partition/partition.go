// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package partition provides common utils for system partition format.
package partition

import (
	"fmt"

	"github.com/siderolabs/go-blockdevice/v2/partitioning/gpt"

	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/imager/quirks"
)

// Options contains the options for creating a partition.
type Options struct {
	FormatOptions

	PartitionLabel string
	PartitionType  Type
	Size           uint64
	PartitionOpts  []gpt.PartitionOption
}

// NewPartitionOptions returns a new PartitionOptions.
//
//nolint:gocyclo
func NewPartitionOptions(label string, uki bool, quirk quirks.Quirks) Options {
	formatOptions := NewFormatOptions(label)
	if formatOptions == nil {
		panic(fmt.Sprintf("unknown format options for label %q", label))
	}

	switch label {
	case constants.EFIPartitionLabel:
		partitionOptions := Options{
			FormatOptions:  *formatOptions,
			PartitionLabel: label,
			PartitionType:  EFISystemPartition,
			Size:           quirk.PartitionSizes().GrubEFISize(),
		}

		if uki {
			partitionOptions.Size = quirk.PartitionSizes().UKIEFISize()
		}

		return partitionOptions
	case constants.BIOSGrubPartitionLabel:
		if uki {
			panic("BIOS partition is not supported with UKI")
		}

		return Options{
			FormatOptions:  *formatOptions,
			PartitionLabel: label,
			PartitionType:  BIOSBootPartition,
			Size:           quirk.PartitionSizes().GrubBIOSSize(),
			PartitionOpts:  []gpt.PartitionOption{gpt.WithLegacyBIOSBootableAttribute(true)},
		}
	case constants.BootPartitionLabel:
		if uki {
			panic("BOOT partition is not supported with UKI")
		}

		return Options{
			FormatOptions:  *formatOptions,
			PartitionLabel: label,
			PartitionType:  LinuxFilesystemData,
			Size:           quirk.PartitionSizes().GrubBootSize(),
		}
	case constants.MetaPartitionLabel:
		return Options{
			FormatOptions:  *formatOptions,
			PartitionLabel: label,
			PartitionType:  LinuxFilesystemData,
			Size:           quirk.PartitionSizes().METASize(),
		}
	case constants.StatePartitionLabel:
		return Options{
			FormatOptions:  *formatOptions,
			PartitionLabel: label,
			PartitionType:  LinuxFilesystemData,
			Size:           quirk.PartitionSizes().StateSize(),
		}
	case constants.EphemeralPartitionLabel:
		return Options{
			FormatOptions:  *formatOptions,
			PartitionLabel: label,
			PartitionType:  LinuxFilesystemData,
			Size:           0,
		}
	case constants.ImageCachePartitionLabel:
		return Options{
			FormatOptions:  *formatOptions,
			PartitionLabel: label,
			PartitionType:  LinuxFilesystemData,
			Size:           0,
		}
	default:
		panic(fmt.Sprintf("unknown partition label %q", label))
	}
}
