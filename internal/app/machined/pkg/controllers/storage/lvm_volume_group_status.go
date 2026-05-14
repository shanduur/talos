// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/safe"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/internal/pkg/lvm"
	"github.com/siderolabs/talos/pkg/machinery/resources/storage"
)

// LVMVolumeGroupStatusController manages LVMVolumeGroupStatus resources.
type LVMVolumeGroupStatusController struct {
	LVM *lvm.LVM
}

// Name implements controller.Controller interface.
func (ctrl *LVMVolumeGroupStatusController) Name() string {
	return "storage.LVMVolumeGroupStatusController"
}

// Inputs implements controller.Controller interface.
func (ctrl *LVMVolumeGroupStatusController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: storage.NamespaceName,
			Type:      storage.LVMPhysicalVolumeStatusType,
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *LVMVolumeGroupStatusController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: storage.LVMVolumeGroupStatusType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *LVMVolumeGroupStatusController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
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

func (ctrl *LVMVolumeGroupStatusController) reconcile(ctx context.Context, r controller.Runtime, _ *zap.Logger) error {
	vgs, err := ctrl.LVM.VGS(ctx)
	if err != nil {
		return fmt.Errorf("vgs: %w", err)
	}

	r.StartTrackingOutputs()

	for _, vg := range vgs {
		if err := safe.WriterModify(ctx, r, storage.NewLVMVolumeGroupStatus(storage.NamespaceName, vg.Name), func(s *storage.LVMVolumeGroupStatus) error {
			spec := s.TypedSpec()
			spec.Name = vg.Name
			spec.UUID = vg.UUID
			spec.Format = vg.Format
			spec.Permissions = vg.Permissions
			spec.Extendable = vg.Extendable
			spec.Exported = vg.Exported
			spec.Partial = vg.Partial
			spec.AllocationPolicy = vg.AllocationPolicy
			spec.Clustered = vg.Clustered
			spec.Shared = vg.Shared
			spec.Size = vg.Size
			spec.Free = vg.Free
			spec.ExtentSize = vg.ExtentSize
			spec.ExtentCount = vg.ExtentCount
			spec.FreeExtentCount = vg.FreeExtentCount
			spec.MaxLV = vg.MaxLV
			spec.MaxPV = vg.MaxPV
			spec.LVCount = vg.LVCount
			spec.PVCount = vg.PVCount
			spec.SnapCount = vg.SnapCount
			spec.MissingPVCount = vg.MissingPVCount
			spec.SeqNo = vg.SeqNo
			spec.LockType = vg.LockType
			spec.SystemID = vg.SystemID
			spec.Tags = []string(vg.Tags)

			return nil
		}); err != nil {
			return fmt.Errorf("modify vg %q: %w", vg.Name, err)
		}
	}

	if err := safe.CleanupOutputs[*storage.LVMVolumeGroupStatus](ctx, r); err != nil {
		return fmt.Errorf("cleanup vg outputs: %w", err)
	}

	return nil
}
