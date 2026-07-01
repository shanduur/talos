// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package talos

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/siderolabs/talos/cmd/talosctl/pkg/talos/global"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/client/multiplex"
)

// mdCmd represents the md command.
var mdCmd = &cobra.Command{
	Use:   "md",
	Short: "Provision and maintain MD (software RAID) arrays",
	Long: `Provision and maintain MD (software RAID) arrays.

This is an administrator command, intended to be run in maintenance mode before
installation to lay down a mirrored system disk:

  - create:  provision a bootable RAID1 mirror from whole disks
  - extend:  add a replacement member
  - shrink:  remove a failed member
  - destroy: tear an array down

mdadm is provided by a system extension and is not part of core Talos; these
commands fail with a precondition error on nodes without the extension.`,
	Args: cobra.NoArgs,
}

var mdCmdFlags struct {
	global.InsecureFlags
}

// mdCreateCmd provisions a bootable RAID1 mirror from whole disks.
var mdCreateCmd = &cobra.Command{
	Use:   "create <name> <disk>...",
	Short: "Provision a bootable RAID1 mirror from whole disks",
	Long: `Provision a bootable RAID1 mirror from whole disks.

The first argument is the array name (e.g. talos); the rest are at least two
whole disks to mirror, as absolute paths (e.g. /dev/sda /dev/sdb).

Each disk is partitioned into an EFI System Partition (kept outside the array so
UEFI can boot from either disk) plus a RAID-member partition; a RAID1 array is
created across the members. ANY EXISTING DATA ON THE DISKS IS WIPED. The
resulting array (printed on success) is the install target.

Example:
  talosctl md create talos /dev/sda /dev/sdb --insecure`,
	Args: cobra.MinimumNArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, disks := args[0], args[1:]

		return withMDClient(cmd.Context(), func(ctx context.Context, c *client.Client) (struct{}, error) {
			resp, err := c.MDCreate(ctx, &machine.MDCreateRequest{
				Name:  name,
				Disks: disks,
			})
			if err != nil {
				return struct{}{}, err
			}

			fmt.Printf("created RAID1 mirror %q at %s (ESPs: %v)\n", name, resp.GetDevice(), resp.GetEspDevices())

			return struct{}{}, nil
		})
	},
}

// mdExtendCmd adds member devices to an array.
var mdExtendCmd = &cobra.Command{
	Use:   "extend <name> <device>...",
	Short: "Add member devices to an MD array",
	Long: `Add member devices to an MD array, growing the number of active devices.

Members must be given as absolute block-device paths (e.g. /dev/sdd).

For RAID1 this increases the mirror count. To replace a failed member, extend
with the replacement device, wait for the resync to finish, then shrink the
failed member out.`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, devices := args[0], args[1:]

		return withMDClient(cmd.Context(), func(ctx context.Context, c *client.Client) (*emptypb.Empty, error) {
			return &emptypb.Empty{}, c.MDExtend(ctx, &machine.MDExtendRequest{
				Name:    name,
				Devices: devices,
			})
		})
	},
}

// mdShrinkCmd removes member devices from an array.
var mdShrinkCmd = &cobra.Command{
	Use:   "shrink <name> <device>...",
	Short: "Remove member devices from an MD array",
	Long: `Remove member devices from an MD array, reducing the number of active devices.

Members must be given as absolute block-device paths (e.g. /dev/sdc).

Each removed member is failed, removed and has its superblock zeroed so the
disk can be reused. Shrinking below one active device is rejected.`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, devices := args[0], args[1:]

		return withMDClient(cmd.Context(), func(ctx context.Context, c *client.Client) (*emptypb.Empty, error) {
			return &emptypb.Empty{}, c.MDShrink(ctx, &machine.MDShrinkRequest{
				Name:    name,
				Devices: devices,
			})
		})
	},
}

// mdDestroyCmd tears an array down.
var mdDestroyCmd = &cobra.Command{
	Use:   "destroy <name>",
	Short: "Destroy an MD array",
	Long: `Stop an MD array and clear the superblock on every member device.

WARNING: this is destructive. The array must not be in use (mounted or claimed
by another device).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		return withMDClient(cmd.Context(), func(ctx context.Context, c *client.Client) (*emptypb.Empty, error) {
			return &emptypb.Empty{}, c.MDDestroy(ctx, &machine.MDDestroyRequest{
				Name: name,
			})
		})
	},
}

// withMDClient runs an MDService call against every selected node, fanning out
// client-side via multiplex because the unary MDService responses are not
// augmented by apid's one-to-many proxy path.
func withMDClient[T any](ctx context.Context, initiate func(context.Context, *client.Client) (T, error)) error {
	clientFactory, err := NewClientFactory(ctx, &mdCmdFlags)
	if err != nil {
		return err
	}

	defer clientFactory.Close() //nolint:errcheck

	respCh := multiplex.UnaryViaFactory(ctx, clientFactory, initiate)

	var errs error

	for resp := range respCh {
		if resp.Err != nil {
			errs = errors.Join(errs, fmt.Errorf("error from node %s: %w", resp.Node, resp.Err))
		}
	}

	return errs
}

func init() {
	addCommand(mdCmd)

	for _, c := range []*cobra.Command{mdCreateCmd, mdExtendCmd, mdShrinkCmd, mdDestroyCmd} {
		mdCmdFlags.InsecureFlags.AddFlags(c)
	}

	mdCmd.AddCommand(mdCreateCmd, mdExtendCmd, mdShrinkCmd, mdDestroyCmd)
}
