# Design: Declarative RAID Volumes (`RAIDVolumeConfig`)

Status: Draft / Design Proposal

Declarative provisioning of Linux MD (software RAID) arrays from machine
config, following the LVM declarative model. Companion to
[boot-from-md.md](boot-from-md.md), which uses this mechanism to provision the
mirrored system disk; this document covers the general array-provisioning
layer, which also serves user-managed data arrays.

## 1. Model

Config document → desired-state spec resource → reconcile controller → status,
the same shape as `LVMVolumeGroupConfig → LVMVolumeGroupSpec →
LVMVolumeGroupReconcileController`:

```
RAIDVolumeConfig (v1alpha1, multi-doc)
        │  render
        ▼
MDArraySpec         (desired state)
        │  reconcile: partitionMirrorMember + md.Create (additive, idempotent)
        ▼
/dev/disk/by-id/md-name-<name>
        │  observe
        ▼
MDArrayStatus       (observed state)
```

The reconcile is **additive only** — it creates arrays and adds members. It
never destroys. Destruction / failed-member removal is the removal-only gRPC
API (`talosctl wipe md` / `wipe md-member`), exactly as LVM splits declarative
provisioning from `talosctl wipe {lv,vg,pv}`.

## 2. `RAIDVolumeConfig` document

Standalone `v1alpha1`, multi-doc (one document per array), modeled on
`LVMVolumeGroupConfig`. Standalone rather than folded into
`UnattendedInstallConfig` because arrays are not tied to the install disk: the
same document type provisions the system mirror *and* user data arrays, and it
stays meaningful on an already-installed node where no unattended install runs.

Fields:

* `name` — array name, stamped into the md metadata. Exposed as
  `/dev/disk/by-id/md-name-<name>`.
* `level` — RAID level. **RAID1 only** for v1.
* `diskSelector` — CEL expression matching the member disks (≥2), reusing the
  existing disk-locator CEL env + `MatchDisks` (returns all matches).

Sketch:

```yaml
apiVersion: v1alpha1
kind: RAIDVolumeConfig
name: talos
level: raid1
diskSelector:
  match: disk.transport == "nvme" && disk.size > 100u * GiB
```

Validation: `level` in the supported set (raid1); `diskSelector.match`
non-empty and parses against the disk locator env. Member count (≥2) is a
runtime reconcile condition, not a static validation (disks are discovered at
runtime).

## 3. Reconcile controller

`MDArrayReconcileController` converges each `MDArraySpec`:

1. Resolve members via `MatchDisks(diskSelector)`. Fewer than 2 → wait (park,
   re-run on the next block event), the same way the LVM reconcile tolerates
   status-scan lag.
2. If the array already exists (`MDArrayStatus` / by-id device present) →
   nothing to do, or add any newly-matched members (`md.Extend`, additive) for
   the disk-replacement path.
3. Otherwise, for each member disk lay down the member layout
   (`partitionMirrorMember`: plain ESP + trailing `LinuxRAID` partition), then
   `md.Create(name, level, n, members)` across the member partitions.
4. Publish `MDArrayStatus`.

Idempotent and additive throughout, mirroring
`LVMVolumeGroupReconcileController` (which no-ops when the observed PV/VG
already matches and only ever creates/extends). Container mode is a no-op (no
devices). `mdadm` is core, so no `ErrNotInstalled` guard is needed beyond cheap
defense.

**Wiping is out of scope here** — a member disk carrying stale data or an old
superblock is the operator's problem, cleared via `talosctl wipe`. The
reconcile never wipes a disk it did not just partition, so it cannot silently
eat data.

## 4. Consuming a provisioned array

* **System mirror** (see boot-from-md.md): the operator points the
  `UnattendedInstallConfig` install selector at
  `/dev/disk/by-id/md-name-<name>` for the array named in its
  `RAIDVolumeConfig`. Install ordering is gated naturally — the install
  controller parks `Pending` until the array device appears. The array carries
  a GPT-on-md with the labeled Talos system partitions; the bootloader is
  fanned out to each member's ESP.
* **Data array**: no `UnattendedInstallConfig` involved. The array is consumed
  as a Talos user volume (a filesystem / `UserVolumeConfig` on top of the md),
  provisioned and mounted like any other block device.

## 5. Scope

**v1:** RAID1 only; standalone `RAIDVolumeConfig` v1alpha1 multi-doc; additive
reconcile (`MDArrayReconcileController`); removal via `talosctl wipe md` /
`wipe md-member`.

**Later:** other RAID levels; grow/reshape; declarative member replacement
(reconcile drops a failed member itself rather than requiring `talosctl wipe
md-member` first).

## 6. Implementation checklist

* `RAIDVolumeConfig` v1alpha1 type + registration + validation + docs (model on
  `LVMVolumeGroupConfig`).
* `MDArraySpec` / `MDArrayStatus` COSI resources.
* Config→spec controller (render `RAIDVolumeConfig` docs → `MDArraySpec`).
* `MDArrayReconcileController` (`partitionMirrorMember` + `md.Create` /
  `md.Extend`; additive, idempotent; container-mode no-op).
* Regen generated files (`*.pb.go`, deepcopy, config docs, schema).
