// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/safe"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/internal/pkg/lvm"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/storage"
)

// LVMPhysicalVolumeStatusController manages LVMPhysicalVolumeStatus resources.
type LVMPhysicalVolumeStatusController struct {
	LVM *lvm.LVM
}

// Name implements controller.Controller interface.
func (ctrl *LVMPhysicalVolumeStatusController) Name() string {
	return "storage.LVMPhysicalVolumeStatusController"
}

// Inputs implements controller.Controller interface.
func (ctrl *LVMPhysicalVolumeStatusController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: block.NamespaceName,
			Type:      block.DiscoveredVolumeType,
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *LVMPhysicalVolumeStatusController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: storage.LVMPhysicalVolumeStatusType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *LVMPhysicalVolumeStatusController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		case <-ticker.C:
		}

		if err := ctrl.reconcile(ctx, r, logger); err != nil {
			return err
		}
	}
}

func (ctrl *LVMPhysicalVolumeStatusController) reconcile(ctx context.Context, r controller.Runtime, _ *zap.Logger) error {
	pvs, err := ctrl.LVM.PVS(ctx)
	if err != nil {
		return fmt.Errorf("pvs: %w", err)
	}

	r.StartTrackingOutputs()

	for _, pv := range pvs {
		// `pvs -a` enumerates every block device on the host; rows that are
		// not actual LVM PVs come back with an empty UUID. Skip them so we
		// don't publish a resource per loop/disk-partition device.
		if pv.UUID == "" {
			continue
		}

		id := pvID(pv.Device)

		if err := safe.WriterModify(ctx, r, storage.NewLVMPhysicalVolumeStatus(storage.NamespaceName, id), func(s *storage.LVMPhysicalVolumeStatus) error {
			spec := s.TypedSpec()
			spec.Device = pv.Device
			spec.VGName = pv.VGName
			spec.UUID = pv.UUID
			spec.Format = pv.Format
			spec.Allocatable = pv.Allocatable
			spec.Exported = pv.Exported
			spec.Missing = pv.Missing
			spec.InUse = pv.InUse
			spec.Size = pv.Size
			spec.DeviceSize = pv.DeviceSize
			spec.Free = pv.Free
			spec.Used = pv.Used
			spec.PECount = pv.PECount
			spec.PEAllocCount = pv.PEAllocCount
			spec.Major = pv.Major
			spec.Minor = pv.Minor
			spec.Tags = []string(pv.Tags)

			return nil
		}); err != nil {
			return fmt.Errorf("modify pv %q: %w", pv.Device, err)
		}
	}

	if err := safe.CleanupOutputs[*storage.LVMPhysicalVolumeStatus](ctx, r); err != nil {
		return fmt.Errorf("cleanup pv outputs: %w", err)
	}

	return nil
}

// pvID derives a resource ID from a PV device path. /dev/sda1 -> sda1.
func pvID(device string) string {
	return strings.TrimPrefix(strings.ReplaceAll(device, "/", "-"), "-dev-")
}
