// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package mdd implements machine.MDService.
package mdd

import (
	"context"
	"errors"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/internal/pkg/md"
	"github.com/siderolabs/talos/pkg/grpc/middleware/authz"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/role"
)

// Service implements machine.MDService.
type Service struct {
	machine.UnimplementedMDServiceServer

	controller runtime.Controller
	logger     *zap.Logger
}

// NewService creates a new MDService.
func NewService(controller runtime.Controller, logger *zap.Logger) *Service {
	return &Service{
		controller: controller,
		logger:     logger.With(zap.String("service", "mdd")),
	}
}

// authorize enforces the same role policy as the LVM and BlockDeviceWipe APIs:
// Admin role is required unless the node is in maintenance mode.
func (svc *Service) authorize(ctx context.Context) error {
	roles := authz.GetRoles(ctx)
	inMaintenance := !svc.controller.Runtime().ConfigCompleteForBoot()

	if !inMaintenance && !roles.Includes(role.Admin) {
		return authz.ErrNotAuthorized
	}

	return nil
}

// mdStatus maps an md-package sentinel to a gRPC status. The underlying mdadm
// stderr is intentionally not surfaced — only well-known sentinels are
// reported with structured codes; everything else collapses to Internal with
// a generic message.
//
// ErrNotInstalled maps to FailedPrecondition: mdadm ships in a system
// extension and is not part of core Talos, so the operation cannot proceed
// until the extension is installed.
func mdStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, md.ErrNotInstalled):
		return status.Error(codes.FailedPrecondition, md.ErrNotInstalled.Error())
	case errors.Is(err, md.ErrNotFound):
		return status.Error(codes.NotFound, md.ErrNotFound.Error())
	case errors.Is(err, md.ErrInUse):
		return status.Error(codes.FailedPrecondition, md.ErrInUse.Error())
	case errors.Is(err, md.ErrExists):
		return status.Error(codes.AlreadyExists, md.ErrExists.Error())
	case errors.Is(err, md.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, md.ErrInvalidArgument.Error())
	default:
		return status.Error(codes.Internal, "md operation failed")
	}
}

// logFailure emits a structured warning capturing the sentinel error AND the
// raw mdadm exit code / stderr (when present) so operators can diagnose the
// failure from machined logs without those details leaking back to the API
// client through the gRPC status message.
func (svc *Service) logFailure(op string, fields []zap.Field, err error) {
	all := make([]zap.Field, 0, len(fields)+4)
	all = append(all, zap.String("op", op))
	all = append(all, fields...)
	all = append(all, zap.Error(err))

	if exec, ok := errors.AsType[*md.ExecError](err); ok {
		all = append(
			all,
			zap.Int("mdadm_exit_code", exec.ExitCode),
			zap.ByteString("mdadm_stderr", exec.Stderr),
		)
	}

	svc.logger.Error("md operation failed", all...)
}

// Create provisions a bootable RAID1 mirror from the given whole disks: each
// disk is partitioned (ESP + RAID member) and the array is created across the
// member partitions.
func (svc *Service) Create(ctx context.Context, req *machine.MDCreateRequest) (*machine.MDCreateResponse, error) {
	if err := svc.authorize(ctx); err != nil {
		return nil, err
	}

	// Create wipes whole disks and only makes sense before installation; refuse
	// it on a configured (booted) node even for Admin to avoid destroying a
	// running system's disks.
	if svc.controller.Runtime().ConfigCompleteForBoot() {
		return nil, status.Error(codes.FailedPrecondition, "md create is only available in maintenance mode")
	}

	name, disks := req.GetName(), req.GetDisks()

	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name must be set")
	}

	if len(disks) < 2 {
		return nil, status.Error(codes.InvalidArgument, "at least two disks are required for a RAID1 mirror")
	}

	mdInst, err := md.New()
	if err != nil {
		return nil, mdStatus(err)
	}

	fields := []zap.Field{zap.String("name", name), zap.Strings("disks", disks)}
	svc.logger.Info("provisioning MD mirror", fields...)

	var (
		espDevices    = make([]string, 0, len(disks))
		memberDevices = make([]string, 0, len(disks))
	)

	for _, disk := range disks {
		esp, member, err := partitionMirrorMember(disk)
		if err != nil {
			svc.logger.Error("failed to partition mirror member", zap.String("disk", disk), zap.Error(err))

			return nil, status.Errorf(codes.Internal, "failed to partition %s", disk)
		}

		espDevices = append(espDevices, esp)
		memberDevices = append(memberDevices, member)
	}

	device, err := mdInst.Create(ctx, name, 1, len(memberDevices), memberDevices)
	if err != nil {
		svc.logFailure("create", fields, err)

		return nil, mdStatus(err)
	}

	return &machine.MDCreateResponse{
		Device:     device,
		EspDevices: espDevices,
	}, nil
}

// Extend adds member devices to an existing array.
func (svc *Service) Extend(ctx context.Context, req *machine.MDExtendRequest) (*emptypb.Empty, error) {
	return svc.mutate(
		ctx, "extend", req.GetName(), req.GetDevices(),
		func(m *md.MD, name string, devices []string) error {
			return m.Extend(ctx, name, devices)
		},
	)
}

// Shrink removes member devices from an existing array.
func (svc *Service) Shrink(ctx context.Context, req *machine.MDShrinkRequest) (*emptypb.Empty, error) {
	return svc.mutate(
		ctx, "shrink", req.GetName(), req.GetDevices(),
		func(m *md.MD, name string, devices []string) error {
			return m.Shrink(ctx, name, devices)
		},
	)
}

// Destroy stops the array and clears member superblocks.
func (svc *Service) Destroy(ctx context.Context, req *machine.MDDestroyRequest) (*emptypb.Empty, error) {
	return svc.mutate(
		ctx, "destroy", req.GetName(), nil,
		func(m *md.MD, name string, _ []string) error {
			return m.Destroy(ctx, name)
		},
	)
}

// mutate is the shared skeleton for Extend/Shrink/Destroy: authorize, validate
// name, require devices for the device-taking ops, init mdadm, run the action
// and normalise errors.
func (svc *Service) mutate(ctx context.Context, op, name string, devices []string, action func(*md.MD, string, []string) error) (*emptypb.Empty, error) {
	if err := svc.authorize(ctx); err != nil {
		return nil, err
	}

	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name must be set")
	}

	// destroy passes a nil device slice; the device-taking ops require one.
	if devices != nil && len(devices) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one device must be set")
	}

	mdInst, err := md.New()
	if err != nil {
		return nil, mdStatus(err)
	}

	fields := []zap.Field{zap.String("name", name)}
	if devices != nil {
		fields = append(fields, zap.Strings("devices", devices))
	}

	svc.logger.Info("mutating MD array", append([]zap.Field{zap.String("op", op)}, fields...)...)

	if err := action(mdInst, name, devices); err != nil {
		svc.logFailure(op, fields, err)

		return nil, mdStatus(err)
	}

	return &emptypb.Empty{}, nil
}
