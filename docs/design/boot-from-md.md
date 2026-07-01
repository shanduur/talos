# Design: Mirroring the Talos System Disk with Linux MD RAID1

Status: Draft / Design Proposal

f7c2d88e-442a-4bdd-9983-e86b43d6393b

## 0. What changed since the first draft

Two platform shifts collapse most of the original complexity:

1. **`mdadm` is always present.** It is no longer shipped as a system
   extension; the binary lives in core Talos in every boot mode (maintenance
   UKI and installed system). There is no `ErrNotInstalled` precondition to
   design around, and no "bind-mount the extension into the installer
   container" hack.
2. **Install is controller-driven, not sequencer-driven.**
   `UnattendedInstallConfig` triggers the install *after* the controllers are
   running (see `runtime.UnattendedInstallController`), not as a sequencer
   phase. Because controllers are already up when the install fires, a
   controller can **provision the RAID array before** the install runs, and
   the install simply targets the array that already exists.

Consequences for this design:

* **The array is provisioned declaratively by a controller**, from config, at
  runtime — not by the installer and not by an imperative gRPC `Create`.
* **The gRPC API is removal-only**, exactly mirroring `LVMService` /
  `talosctl wipe {lv,vg,pv}`: it exists to tear down an mdadm-provisioned
  array (or drop a failed member), not to create one.
* The old maintenance-mode dance (`talosctl md create` → `gen config
  --install-disk=<array>` → `apply-config`) is gone. You inject one config
  bundle; the controllers create the mirror and install onto it.

The conceptual sections (§1–§7) are unchanged physics — UEFI can't boot from
an MD member, ESPs must be duplicated, members must never be mounted raw. The
mechanism sections (§8–§12) are rewritten for the controller model.

---

## 1. Overview

The requirement is usually stated as:

> "Boot Talos from an MD device."

For UKI-based booting that is not accurate. Talos does not boot from a root
filesystem on `/dev/md*`:

1. UEFI firmware loads a Unified Kernel Image (UKI) from an EFI System
   Partition (ESP).
2. The UKI contains the kernel, initramfs, and Talos squashfs image.
3. The OS runs from the embedded squashfs in memory.
4. Persistent state is stored separately on disk.

The real requirement is:

> Survive the loss of a single system disk without losing the node.

Which decomposes into:

* **Firmware boot path** — the system must remain bootable after losing one
  disk. Solved by duplicated ESPs (§4), never by MD.
* **Persistent storage** — `META`/`STATE`/`EPHEMERAL` must remain available
  after losing one disk. Solved by MD RAID1 (§6).

The firmware never boots directly from an MD device. MD is purely a redundancy
mechanism for persistent storage.

---

## 2. Talos Storage Model

Talos is nearly stateless. The OS is delivered by the UKI and runs from memory.
Persistent storage is limited to:

* `META` — machine metadata, install info; adjacent to STATE.
* `STATE` — machine config, cluster secrets, persistent system state.
* `EPHEMERAL` — `/var`, container images, logs, etcd data, local PVs.

Only `META`, `STATE`, and `EPHEMERAL` need mirroring. The UKI does not.

---

## 3. Boot Constraints (UEFI)

UEFI firmware can only load EFI executables from a discoverable FAT EFI System
Partition. Firmware cannot assemble MD arrays, understand
`linux_raid_member`, or discover `metadata=1.2` arrays. Therefore the ESP
cannot live on a normal MD array.

---

## 4. ESP Design: Duplicated ESPs

Each disk receives an independent, plain (non-MD) ESP:

```
/dev/sda1 -> FAT32 ESP
/dev/sdb1 -> FAT32 ESP
```

Each ESP holds `EFI/BOOT/BOOTX64.EFI` (and optionally `EFI/Linux/Talos.efi`).
The installer copies the same UKI to every ESP and writes a UEFI boot entry
for each disk.

Boot flow:

```
UEFI → ESP on sda1 OR sdb1 → Talos UKI → kernel + initramfs + squashfs
     → assemble MD arrays → mount persistent partitions
```

Disk failure is tolerated because firmware can boot from either disk
independently. No MD array participates in the firmware boot path.

### Why not a metadata=1.0 ESP array?

A RAID1 ESP with `metadata=1.0` is technically possible (superblock at the end,
each member a valid FAT fs), but it buys nothing for UKI boot and adds
complexity (firmware reads a degraded member directly, ESP writes traverse MD,
resync semantics get murky). Duplicated ESPs are simpler and more robust.

---

## 5. On-Disk Layout: GPT-on-md, single array

Per member disk: a plain ESP + one trailing `LinuxRAID` partition spanning the
rest. One RAID1 array is created across the member partitions. A **GPT is laid
on top of the md**, carrying the labeled Talos system partitions.

```
sda1  ESP (plain, bootable)          sdb1  ESP (plain, bootable)
sda2  LinuxRAID member  ─┐    ┌─ sdb2  LinuxRAID member
                         ├─ /dev/md/talos (RAID1)
                         │     GPT-on-md:
                         │       META / STATE / EPHEMERAL (labeled partitions)
```

**Why GPT-on-md and not one array per partition** (this supersedes the earlier
"one MD array per Talos partition" idea): Talos locates its system volumes by
GPT `partition_label` (`volumeconfig/types.go` `metaMatch`/`labelVolumeMatch`).
A bare filesystem sitting directly on a per-partition array carries no GPT
label, so the volume locators would never find it — locating them would need a
locator rewrite. With a GPT on top of a single md, the labeled partitions exist
exactly as on a normal disk, and the volume locators plus `SystemDiskController`
(which keys off the META partition's parent → resolves to the md) need **no
changes**.

The stable reference to the array is the udev by-id symlink
`/dev/disk/by-id/md-name-talos` (Talos populates no `/dev/md/` alias dir, so the
kernel auto-numbers the node, e.g. `/dev/md127`).

### 5.1 Critical invariant: never mount an MD member raw

> Talos must never mount `META`, `STATE`, or `EPHEMERAL` directly from a
> `linux_raid_member` device.

If `/dev/sda2` were discovered and mounted before `md/talos` is assembled, the
node would bypass RAID entirely: boot from half the mirror, diverging writes,
corruption after reassembly, redundancy silently lost. Requirements:

1. MD assembly happens before the `VolumeManager` resolves system volumes.
2. Devices identified as raid members are excluded from direct partition/fs
   discovery.
3. Locators prefer the assembled `/dev/md/*` over its members.

How the invariant is actually enforced today (given tooling limits — see §11):

* **The GPT-on-md layout puts the fs offset inside the md data area.** With
  `metadata=1.2` the md superblock sits at 4K and the member partition is typed
  `LinuxRAID` (GUID `A19D880F-…`), not a Talos volume label. There is no
  labeled Talos partition on the raw member to discover — the labels only exist
  in the GPT that lives *on* the md. So a raw-member probe finds nothing to
  mount by label.
* Assembly liveness (arrays actually come up, degraded included) is guaranteed
  by udev incremental assembly + `MDLastResortController` (§8), not by the
  locator.

`go-blockdevice` blkid has no md probe, so Talos cannot positively detect a
`linux_raid_member` and hide it. The safety therefore rests on the layout
(labels live only on the md), not on member-exclusion logic. Documented as a
known gap in §11.

---

## 6. MD metadata version

All arrays use `metadata=1.2` (mdadm's default). `metadata=1.0` gives no
benefit here: firmware never reads these arrays, assembly is userspace,
activation happens after the UKI boots.

---

## 7. Runtime assembly & degraded boot

Healthy assembly is automatic: the udev rules run `mdadm --incremental` per
uevent, and Talos's udevd `trigger`+`settle` brings assembled arrays up before
volume resolution. No assemble-scan controller is needed (one was built, then
removed as redundant).

Degraded boot needs help. Talos has no systemd, so `mdadm-last-resort@.timer`
never fires — a RAID1 missing a member is left `inactive` by incremental
assembly, waiting forever for the absent disk. **`MDLastResortController`**
(`controllers/storage/md_last_resort.go`) is the replacement: it gates on udevd
healthy, finds `array_state == inactive` arrays via sysfs
(`md.InactiveArrays()`, no mdadm needed), waits a grace period (default 30s,
overridable), then force-runs each still-inactive array with `mdadm --run`
(`md.RunArray`). Only degraded boots pay the grace; healthy / no-md nodes never
delay.

---

## 8. Declarative provisioning (the array creation path)

Array creation is a **controller reconcile from config** — a standalone
`RAIDVolumeConfig` v1alpha1 document → `MDArraySpec` →
`MDArrayReconcileController` → `MDArrayStatus`, modeled on the LVM declarative
layer. The full design (document schema, reconcile behavior, data-array use)
lives in [raid-volumes.md](raid-volumes.md). This section covers only the bits
specific to the **system mirror**.

The array appears as `/dev/disk/by-id/md-name-<name>` once
`MDArrayReconcileController` has created and assembled it. The reconcile is
**additive only**; destruction / failed-member removal is the gRPC removal API
(§9).

**Ordering with install — no explicit dependency needed.** The install disk
selector on `UnattendedInstallConfig` matches the *array* device, which only
exists once `MDArrayReconcileController` created and assembled it. Until then,
`UnattendedInstallController` finds no matching disk and parks in
`Pending` (it already does this). When the md device appears, the selector
matches and the install runs onto the array. Disk-appearance is the natural
gate; the two controllers need no wiring between them.

**Installer consume-side (unchanged, already built).** When the install target
is an md device, the installer auto-detects it (`isMDDevice` via sysfs
`/sys/block/<mdN>/md`) and switches to the mirror path: the array carries only
the GPT-on-md system partitions (META at install; STATE/EPHEMERAL provisioned
at runtime onto the array as the system disk), and the bootloader is fanned out
to each member disk's ESP (`installMirrorBootloader` → member disks from
`/sys/block/<mdN>/slaves` → per-member format ESP + `installBootloader` + UEFI
boot entry). **No mdadm runs in the installer** — the array already exists.

---

## 9. Removal API (gRPC, removal-only)

`MDService` mirrors `LVMService`: it exists to **remove** mdadm-provisioned
storage, not create it. Creation is declarative (§8); the API is the imperative
teardown / repair path, surfaced through `talosctl wipe`.

Kept operations:

* **Destroy array** — `--stop` the array + `--zero-superblock` every member so
  the disks are reusable (`md.Destroy`). This is the direct analog of
  `LVMService.VolumeGroupRemove`.
* **Remove member** — fail + remove a member and zero its superblock
  (`md.Shrink` on a single device), for pulling a failed disk out of the
  mirror before its replacement is added back by the reconcile. Analog of
  `LVMService.PhysicalVolumeRemove`.

Dropped: `MDService.Create` (and its `MDCreateRequest`/`Response`,
`MDLevel` enum). Array creation is the controller's job. `internal/pkg/md`
keeps `Create`/`Extend` — the reconcile controller calls them directly, not
over gRPC.

`talosctl` surface, matching `talosctl wipe {lv,vg,pv}`:

```
talosctl wipe md <name>                # destroy the whole array
talosctl wipe md-member <name> <dev>   # drop a single (failed) member
```

Authorization mirrors the LVM / BlockDeviceWipe policy: Admin role required
unless the node is in maintenance mode. Errors are normalized to sanitized gRPC
statuses; raw mdadm stderr is logged server-side only.

---

## 10. Failure & replacement

**Loss of one disk:** firmware boots from the surviving ESP; the array
assembles degraded (`MDLastResortController` force-runs it after the grace);
the node stays operational.

**Replacement disk:** partition the replacement (ESP + `LinuxRAID` member) and
add it back. The reconcile controller re-adds the member (`md.Extend`, additive
path) and the array resyncs; copy the UKI onto the new ESP. If the old failed
member is still attached, drop it first with `talosctl wipe md-member`.

---

## 11. Scope & open risks

**In scope (v1):** RAID1 only; duplicated plain ESP per disk; `metadata=1.2`;
single GPT-on-md array for the system disk; declarative provisioning via
config + reconcile controller; removal-only gRPC (`talosctl wipe md`).

**Open risks / unverified:**

* **P0 — kernel md modules in the UKI initramfs.** `md_mod` + `raid1` must be
  present in the boot UKI's initramfs; core `mdadm` is userspace only. Confirm
  in `pkgs`.
* **Raw-member detection gap.** `go-blockdevice` blkid has no md probe, so
  §5.1's "exclude members from discovery" cannot be positively enforced;
  safety rests on the layout (Talos labels live only in the GPT-on-md, never on
  raw members) + `metadata=1.2` data offset. Revisit if an upstream blkid md
  probe lands, or enforce via `LinuxRAID` partition-type-GUID exclusion in the
  volume locators.
* **Upgrades.** `ModeUpgrade` reuses partitions; the upgrade path must re-sync
  the UKI to *every* member ESP and leave the array untouched. Needs a dedicated
  slice.

---

## 12. Implementation Progress (living section)

Maintained by the implementation work; updated as slices land. Where it
disagrees with §1–§11, the decisions here are newer.

### Architecture (current, post platform shift)

* Provisioning is **controller-driven declarative** (§8), gated behind
  `UnattendedInstallConfig`-triggered install. No sequencer install task, no
  imperative `md create`.
* gRPC `MDService` is **removal-only** (§9): destroy array + drop member,
  surfaced as `talosctl wipe md` / `wipe md-member`. Mirrors `LVMService`.
* `mdadm` is **core** (always present), not a system extension. The
  `ErrNotInstalled → FailedPrecondition` mapping stays as cheap defense but is
  effectively unreachable now; the `/usr/local` installer bind-mount is dead.

### Done (carried over, still valid)

* `internal/pkg/md` — mdadm wrapper: `Create/Extend/Shrink/Destroy/Detail`,
  `RunArray`, `InactiveArrays`, by-id device paths.
* `internal/app/mdd/mirror.go` `partitionMirrorMember` — per-member ESP +
  trailing `LinuxRAID` partition.
* `LinuxRAIDPartition` GPT type GUID (`internal/pkg/partition/constants.go`).
* Healthy-array assembly via udev incremental + udevd trigger/settle (no
  controller).
* `MDLastResortController` (`controllers/storage/md_last_resort.go`) — degraded
  boot force-run.
* Installer consume-side: `isMDDevice` auto-detect → GPT-on-md system
  partitions + bootloader fan-out to every member ESP. No mdadm in installer.

### To do / to rework for the new architecture

* **Drop `MDService.Create`** from the proto + handler + `talosctl md create`;
  regen `*.pb.go`. Keep Destroy + add a member-removal RPC.
* **Rename the `talosctl` surface** from `md {create,extend,shrink,destroy}` to
  the removal-only `wipe md` / `wipe md-member` to match `talosctl wipe`.
* **Build the declarative path** — see [raid-volumes.md](raid-volumes.md) §6
  for the full checklist (`RAIDVolumeConfig` type → `MDArraySpec` →
  `MDArrayReconcileController` → `MDArrayStatus`, modeled on the LVM layer).
* **Wire install ordering** by pointing the `UnattendedInstallConfig` install
  selector at the array device; verify the Pending→match→install gate works
  once the md device appears (no explicit controller dependency).
* **Upgrades (§11):** re-sync UKI to every member ESP on `ModeUpgrade`.
* **End-to-end test** on the dev QEMU flow (`--disks=2`): controller creates the
  array, install lands META/STATE/EPHEMERAL on the md, node boots from either
  disk, degraded boot survives a pulled disk.

### Dev build / boot (new flow)

`mdadm` is core, so no `--system-extension-image` needed:

```
make image-metal-uki IMAGE_REGISTRY=127.0.0.1:5005 PUSH=true
make talosctl        IMAGE_REGISTRY=127.0.0.1:5005
```

Boot a node with ≥2 disks and inject a config bundle containing an
`UnattendedInstallConfig` (installer image + install selector = the array) plus
the RAID provisioning config (member `diskSelector`, RAID1). The controllers
create the mirror; `UnattendedInstallController` installs onto it and reboots.

```
sudo --preserve-env=HOME _out/talosctl-linux-amd64 cluster create dev \
    --provisioner=qemu \
    --cidr=172.20.0.0/24 \
    --uki-path=_out/metal-amd64-uki.efi \
    --controlplanes 1 --workers 0 \
    --disk=15360 --disks=2
```

Registry mirrors are still required so the node can pull the installer image
(it reaches the host registry via `172.20.0.1`, not `127.0.0.1`); supply them
in the injected config, one `RegistryMirrorConfig` doc per registry.

To test degraded boot: stop the cluster, detach one disk, start the node — it
should boot from the surviving ESP while `MDLastResortController` force-runs the
degraded array.
