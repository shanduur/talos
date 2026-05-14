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

// LVMLogicalVolumeStatusController manages LVMLogicalVolumeStatus resources.
type LVMLogicalVolumeStatusController struct {
	LVM *lvm.LVM
}

// Name implements controller.Controller interface.
func (ctrl *LVMLogicalVolumeStatusController) Name() string {
	return "storage.LVMLogicalVolumeStatusController"
}

// Inputs implements controller.Controller interface.
func (ctrl *LVMLogicalVolumeStatusController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: block.NamespaceName,
			Type:      block.DiscoveredVolumeType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: storage.NamespaceName,
			Type:      storage.LVMPhysicalVolumeStatusType,
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *LVMLogicalVolumeStatusController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: storage.LVMLogicalVolumeStatusType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *LVMLogicalVolumeStatusController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
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

func (ctrl *LVMLogicalVolumeStatusController) reconcile(ctx context.Context, r controller.Runtime, _ *zap.Logger) error {
	lvs, err := ctrl.LVM.LVS(ctx)
	if err != nil {
		return fmt.Errorf("lvs: %w", err)
	}

	r.StartTrackingOutputs()

	for _, lv := range lvs {
		// Hidden / internal LVs (lv_role contains "private") have no lv_path;
		// fall back to the qualified name so they still get a stable ID.
		key := lv.FullName
		if key == "" {
			key = lv.Path
		}

		if err := safe.WriterModify(ctx, r, storage.NewLVMLogicalVolumeStatus(storage.NamespaceName, lvID(key)), func(s *storage.LVMLogicalVolumeStatus) error {
			spec := s.TypedSpec()
			spec.Path = lv.Path
			spec.DMPath = lv.DMPath
			spec.Name = lv.Name
			spec.FullName = lv.FullName
			spec.VGName = lv.VGName
			spec.UUID = lv.UUID
			spec.Layout = lv.Layout
			spec.Role = lv.Role
			spec.Permissions = lv.Permissions
			spec.AllocationPolicy = lv.AllocationPolicy
			spec.AllocationLocked = lv.AllocationLocked
			spec.FixedMinor = lv.FixedMinor
			spec.Active = lv.Active
			spec.ActiveLocally = lv.ActiveLocally
			spec.ActiveRemotely = lv.ActiveRemotely
			spec.ActiveExclusively = lv.ActiveExclusively
			spec.Suspended = lv.Suspended
			spec.DeviceOpen = lv.DeviceOpen
			spec.SkipActivation = lv.SkipActivation
			spec.Merging = lv.Merging
			spec.Converting = lv.Converting
			spec.Size = lv.Size
			spec.MetadataSize = lv.MetadataSize
			spec.ReadAhead = lv.ReadAhead
			spec.KernelMajor = lv.KernelMajor
			spec.KernelMinor = lv.KernelMinor
			spec.Origin = lv.Origin
			spec.OriginSize = lv.OriginSize
			spec.PoolLV = lv.PoolLV
			spec.DataLV = lv.DataLV
			spec.MetadataLV = lv.MetadataLV
			spec.MovePV = lv.MovePV
			spec.ConvertLV = lv.ConvertLV
			spec.WhenFull = lv.WhenFull
			spec.Tags = []string(lv.Tags)

			return nil
		}); err != nil {
			return fmt.Errorf("modify lv %q: %w", key, err)
		}
	}

	if err := safe.CleanupOutputs[*storage.LVMLogicalVolumeStatus](ctx, r); err != nil {
		return fmt.Errorf("cleanup lv outputs: %w", err)
	}

	return nil
}

// lvID derives a resource ID from an LV path or full name.
// "/dev/vg0/data" -> "vg0-data"; "vg0/data" -> "vg0-data".
func lvID(key string) string {
	return strings.TrimPrefix(strings.ReplaceAll(key, "/", "-"), "-dev-")
}
