// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package volumes_test

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/block/internal/volumes"
	blockpb "github.com/siderolabs/talos/pkg/machinery/api/resource/definitions/block"
	taloscel "github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

func TestLocateAndProvision(t *testing.T) {
	// Helpers to reduce boilerplate in test table
	mkCEL := func(expr string, env *cel.Env) taloscel.Expression {
		return taloscel.MustExpression(taloscel.ParseBooleanExpression(expr, env))
	}

	mkDisk := func(dev string, size uint64, opts ...func(*blockpb.DiskSpec)) volumes.DiskContext {
		d := &blockpb.DiskSpec{DevPath: dev, Size: size}
		for _, opt := range opts {
			opt(d)
		}

		return volumes.DiskContext{Disk: d}
	}

	mkVol := func(dev string, parent string, size uint64, opts ...func(*blockpb.DiscoveredVolumeSpec)) *blockpb.DiscoveredVolumeSpec {
		v := &blockpb.DiscoveredVolumeSpec{DevPath: dev, ParentDevPath: parent, Size: size}
		for _, opt := range opts {
			opt(v)
		}

		return v
	}

	// Option helpers
	withLabel := func(l string) func(*blockpb.DiscoveredVolumeSpec) {
		return func(v *blockpb.DiscoveredVolumeSpec) { v.PartitionLabel = l }
	}
	withName := func(n string) func(*blockpb.DiscoveredVolumeSpec) {
		return func(v *blockpb.DiscoveredVolumeSpec) { v.Name = n }
	}
	withUUID := func(u string) func(*blockpb.DiscoveredVolumeSpec) {
		return func(v *blockpb.DiscoveredVolumeSpec) { v.Uuid = u }
	}
	withSerial := func(s string) func(*blockpb.DiskSpec) { return func(d *blockpb.DiskSpec) { d.Serial = s } }
	readOnly := func(d *blockpb.DiskSpec) { d.Readonly = true }

	withPartitionIndex := func(i uint64) func(*blockpb.DiscoveredVolumeSpec) {
		return func(v *blockpb.DiscoveredVolumeSpec) { v.PartitionIndex = i }
	}
	withPartitionUUID := func(u string) func(*blockpb.DiscoveredVolumeSpec) {
		return func(v *blockpb.DiscoveredVolumeSpec) { v.PartitionUuid = u }
	}

	// Constants
	const (
		gb = 1 << 30
		mb = 1 << 20
	)

	//nolint:dupl
	tests := []struct {
		name                string
		volumeConfig        block.VolumeConfigSpec
		discoveredVolumes   []*blockpb.DiscoveredVolumeSpec
		disks               []volumes.DiskContext
		devicesReady        bool
		prevWaveProvisioned bool
		expectedPhase       block.VolumePhase
		expectedError       string
		assertStatus        func(*testing.T, block.VolumeStatusSpec)
	}{
		// --- Simple Volume Types ---
		{
			name:          "tmpfs volume is always ready",
			volumeConfig:  block.VolumeConfigSpec{Type: block.VolumeTypeTmpfs},
			expectedPhase: block.VolumePhaseReady,
		},
		{
			name: "external volume is ready with location",
			volumeConfig: block.VolumeConfigSpec{
				Type: block.VolumeTypeExternal,
				Provisioning: block.ProvisioningSpec{
					DiskSelector:   block.DiskSelector{External: "/dev/ext0"},
					FilesystemSpec: block.FilesystemSpec{Type: block.FilesystemTypeXFS},
				},
			},
			expectedPhase: block.VolumePhaseReady,
			assertStatus: func(t *testing.T, s block.VolumeStatusSpec) {
				assert.Equal(t, block.FilesystemTypeXFS, s.Filesystem)
				assert.Equal(t, "/dev/ext0", s.Location)
			},
		},

		// --- Validation ---
		{
			name:          "partition with zero locator fails",
			volumeConfig:  block.VolumeConfigSpec{Type: block.VolumeTypePartition},
			expectedError: "volume locator is not set",
		},

		// --- Locator Logic ---
		{
			name: "located via Match expression (Partition)",
			volumeConfig: block.VolumeConfigSpec{
				Type: block.VolumeTypePartition,
				Locator: block.LocatorSpec{
					Match: mkCEL(`volume.partition_label == "STATE"`, celenv.VolumeLocator()),
				},
			},
			discoveredVolumes: []*blockpb.DiscoveredVolumeSpec{
				mkVol("/dev/sda1", "/dev/sda", 1*gb, withLabel("STATE"), withUUID("uuid-123")),
			},
			disks: []volumes.DiskContext{mkDisk("/dev/sda", 10*gb)},

			expectedPhase: block.VolumePhaseLocated,
			assertStatus: func(t *testing.T, s block.VolumeStatusSpec) {
				assert.Equal(t, "/dev/sda1", s.Location)
				assert.Equal(t, "uuid-123", s.UUID)
			},
		},
		{
			name: "located via Match expression (Disk without parent)",
			volumeConfig: block.VolumeConfigSpec{
				Type: block.VolumeTypeDisk,
				Locator: block.LocatorSpec{
					Match: mkCEL(`volume.name == "xfs"`, celenv.VolumeLocator()),
				},
			},
			discoveredVolumes: []*blockpb.DiscoveredVolumeSpec{
				mkVol("/dev/sdb", "", 5*gb, withName("xfs"), withUUID("uuid-disk")),
			},
			disks: []volumes.DiskContext{mkDisk("/dev/sdb", 5*gb)},

			expectedPhase: block.VolumePhaseLocated,
			assertStatus: func(t *testing.T, s block.VolumeStatusSpec) {
				assert.Equal(t, "/dev/sdb", s.Location)
				assert.Equal(t, "", s.ParentLocation)
			},
		},
		{
			name: "located via DiskMatch expression",
			volumeConfig: block.VolumeConfigSpec{
				Type: block.VolumeTypePartition,
				Locator: block.LocatorSpec{
					DiskMatch: mkCEL(`disk.serial == "SERIAL001"`, celenv.DiskLocator()),
				},
			},
			discoveredVolumes: []*blockpb.DiscoveredVolumeSpec{
				mkVol("/dev/sda1", "/dev/sda", 2*gb, withLabel("DATA")),
			},
			disks: []volumes.DiskContext{
				mkDisk("/dev/sda", 10*gb, withSerial("SERIAL001")),
			},
			expectedPhase: block.VolumePhaseLocated,
		},

		// --- Waiting / Missing States ---
		{
			name: "volume not located, devices NOT ready -> Waiting",
			volumeConfig: block.VolumeConfigSpec{
				Type: block.VolumeTypePartition,
				Locator: block.LocatorSpec{
					Match: mkCEL(`volume.partition_label == "MISSING"`, celenv.VolumeLocator()),
				},
			},
			devicesReady:  false,
			expectedPhase: block.VolumePhaseWaiting,
		},
		{
			name: "volume not located, devices ready, NO provisioning spec -> Missing",
			volumeConfig: block.VolumeConfigSpec{
				Type: block.VolumeTypePartition,
				Locator: block.LocatorSpec{
					Match: mkCEL(`volume.partition_label == "MISSING"`, celenv.VolumeLocator()),
				},
			},
			devicesReady:  true,
			expectedPhase: block.VolumePhaseMissing,
		},
		{
			name: "volume not located, previous wave NOT provisioned -> Waiting",
			volumeConfig: block.VolumeConfigSpec{
				Type: block.VolumeTypePartition,
				Locator: block.LocatorSpec{
					Match: mkCEL(`volume.partition_label == "MISSING"`, celenv.VolumeLocator()),
				},
				Provisioning: block.ProvisioningSpec{
					DiskSelector: block.DiskSelector{
						Match: mkCEL(`!disk.readonly`, celenv.DiskLocator()),
					},
				},
			},
			devicesReady:        true,
			prevWaveProvisioned: false,
			expectedPhase:       block.VolumePhaseWaiting,
		},

		// --- Provisioning Logic Errors ---
		{
			name: "provisioning: no disks matched selector",
			volumeConfig: block.VolumeConfigSpec{
				Type:    block.VolumeTypePartition,
				Locator: block.LocatorSpec{Match: mkCEL(`false`, celenv.VolumeLocator())}, // Force miss
				Provisioning: block.ProvisioningSpec{
					DiskSelector: block.DiskSelector{
						Match: mkCEL(`disk.serial == "NONEXISTENT"`, celenv.DiskLocator()),
					},
				},
			},
			disks:               []volumes.DiskContext{mkDisk("/dev/sda", 10*gb, withSerial("ACTUAL"))},
			devicesReady:        true,
			prevWaveProvisioned: true,
			expectedError:       "no disks matched selector for volume",
		},
		{
			name: "provisioning: match fail due to readonly",
			volumeConfig: block.VolumeConfigSpec{
				Type:    block.VolumeTypePartition,
				Locator: block.LocatorSpec{Match: mkCEL(`false`, celenv.VolumeLocator())},
				Provisioning: block.ProvisioningSpec{
					DiskSelector: block.DiskSelector{
						Match: mkCEL(`!disk.readonly`, celenv.DiskLocator()),
					},
				},
			},
			disks:               []volumes.DiskContext{mkDisk("/dev/sda", 10*gb, readOnly)},
			devicesReady:        true,
			prevWaveProvisioned: true,
			expectedError:       "no disks matched selector for volume",
		},
		{
			name: "provisioning: multiple disks matched for Disk Volume",
			volumeConfig: block.VolumeConfigSpec{
				Type:    block.VolumeTypeDisk,
				Locator: block.LocatorSpec{Match: mkCEL(`false`, celenv.VolumeLocator())},
				Provisioning: block.ProvisioningSpec{
					DiskSelector: block.DiskSelector{
						Match: mkCEL(`!disk.readonly`, celenv.DiskLocator()),
					},
				},
			},
			disks: []volumes.DiskContext{
				mkDisk("/dev/sda", 10*gb),
				mkDisk("/dev/sdb", 20*gb),
			},
			devicesReady:        true,
			prevWaveProvisioned: true,
			expectedError:       "multiple disks matched selector for disk volume",
		},
		{
			name: "disk volume with DiskMatch locates disk device, not partition",
			volumeConfig: block.VolumeConfigSpec{
				Type: block.VolumeTypeDisk,
				Locator: block.LocatorSpec{
					DiskMatch: mkCEL(`disk.serial == "SN100"`, celenv.DiskLocator()),
				},
			},
			discoveredVolumes: []*blockpb.DiscoveredVolumeSpec{
				// partition entry appears first
				mkVol("/dev/sda1", "/dev/sda", 1*gb, withLabel("EFI")),
				// whole-disk entry appears second
				mkVol("/dev/sda", "", 10*gb),
			},
			disks: []volumes.DiskContext{
				mkDisk("/dev/sda", 10*gb, withSerial("SN100")),
			},
			expectedPhase: block.VolumePhaseLocated,
			assertStatus: func(t *testing.T, s block.VolumeStatusSpec) {
				assert.Equal(t, "/dev/sda", s.Location, "disk volume should locate the disk device, not a partition")
				assert.Equal(t, "", s.ParentLocation, "disk volume should have no parent location")
			},
		},
		{
			name: "partition volume with DiskMatch locates partition, not whole-disk",
			volumeConfig: block.VolumeConfigSpec{
				Type: block.VolumeTypePartition,
				Locator: block.LocatorSpec{
					DiskMatch: mkCEL(`disk.serial == "SN200"`, celenv.DiskLocator()),
				},
			},
			discoveredVolumes: []*blockpb.DiscoveredVolumeSpec{
				// whole-disk entry appears first
				mkVol("/dev/sda", "", 10*gb),
				// partition entry appears second
				mkVol("/dev/sda1", "/dev/sda", 1*gb, withLabel("DATA"), withPartitionIndex(1), withPartitionUUID("puuid-1")),
			},
			disks: []volumes.DiskContext{
				mkDisk("/dev/sda", 10*gb, withSerial("SN200")),
			},
			expectedPhase: block.VolumePhaseLocated,
			assertStatus: func(t *testing.T, s block.VolumeStatusSpec) {
				// Partition volumes stop at the first match; since whole-disk also
				// matches DiskMatch on the parent disk, the first discovered volume wins.
				assert.Equal(t, "/dev/sda", s.Location, "partition volume should locate the first matching discovered volume")
			},
		},
		{
			name: "partition volume with DiskMatch and multiple partitions on same disk",
			volumeConfig: block.VolumeConfigSpec{
				Type: block.VolumeTypePartition,
				Locator: block.LocatorSpec{
					DiskMatch: mkCEL(`disk.serial == "SN300"`, celenv.DiskLocator()),
				},
			},
			discoveredVolumes: []*blockpb.DiscoveredVolumeSpec{
				mkVol("/dev/sdb1", "/dev/sdb", 1*gb, withLabel("BOOT"), withPartitionIndex(1), withPartitionUUID("puuid-b1")),
				mkVol("/dev/sdb2", "/dev/sdb", 4*gb, withLabel("ROOT"), withPartitionIndex(2), withPartitionUUID("puuid-b2")),
			},
			disks: []volumes.DiskContext{
				mkDisk("/dev/sdb", 10*gb, withSerial("SN300")),
			},
			expectedPhase: block.VolumePhaseLocated,
			assertStatus: func(t *testing.T, s block.VolumeStatusSpec) {
				assert.Equal(t, "/dev/sdb1", s.Location, "partition volume should locate the first matching partition")
				assert.Equal(t, "/dev/sdb", s.ParentLocation)
				assert.Equal(t, 1, s.PartitionIndex)
				assert.Equal(t, "puuid-b1", s.PartitionUUID)
			},
		},
		{
			name: "partition volume with DiskMatch across multiple disks locates first match",
			volumeConfig: block.VolumeConfigSpec{
				Type: block.VolumeTypePartition,
				Locator: block.LocatorSpec{
					DiskMatch: mkCEL(`!disk.readonly`, celenv.DiskLocator()),
				},
			},
			discoveredVolumes: []*blockpb.DiscoveredVolumeSpec{
				mkVol("/dev/sda1", "/dev/sda", 2*gb, withLabel("ALPHA"), withPartitionIndex(1), withPartitionUUID("puuid-a1")),
				mkVol("/dev/sdb1", "/dev/sdb", 3*gb, withLabel("BETA"), withPartitionIndex(1), withPartitionUUID("puuid-b1")),
			},
			disks: []volumes.DiskContext{
				mkDisk("/dev/sda", 10*gb, withSerial("SN-A")),
				mkDisk("/dev/sdb", 20*gb, withSerial("SN-B")),
			},
			expectedPhase: block.VolumePhaseLocated,
			assertStatus: func(t *testing.T, s block.VolumeStatusSpec) {
				// Partition volumes do NOT fail on multiple disk matches; they just take the first hit
				assert.Equal(t, "/dev/sda1", s.Location, "partition volume should locate the first matching partition across disks")
				assert.Equal(t, "/dev/sda", s.ParentLocation)
				assert.Equal(t, "puuid-a1", s.PartitionUUID)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger := zaptest.NewLogger(t)

			// Setup Config
			volumeCfg := block.NewVolumeConfig(block.NamespaceName, "TEST")
			*volumeCfg.TypedSpec() = test.volumeConfig

			status := block.VolumeStatusSpec{}

			// Build Context
			ctx := volumes.ManagerContext{
				Cfg:                     volumeCfg,
				Status:                  &status,
				DiscoveredVolumes:       test.discoveredVolumes,
				Disks:                   test.disks,
				DevicesReady:            test.devicesReady,
				PreviousWaveProvisioned: test.prevWaveProvisioned,
			}

			// Execute
			err := volumes.LocateAndProvision(t.Context(), logger, ctx)

			// Assertions
			if test.expectedError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.expectedError)
			} else {
				require.NoError(t, err)
				assert.Equal(t, test.expectedPhase, status.Phase)
			}

			if test.assertStatus != nil {
				test.assertStatus(t, status)
			}
		})
	}
}
