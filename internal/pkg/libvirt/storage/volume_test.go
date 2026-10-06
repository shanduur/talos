// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	libvirt "github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"libvirt.org/go/libvirtxml"

	libvirtstorage "github.com/siderolabs/talos/internal/pkg/libvirt/storage"
)

// qcow2Magic is what a qcow2 file starts with. The fake writes it so that a refresh can probe a
// file's format the way libvirt does, rather than trusting a name.
var qcow2Magic = []byte{'Q', 'F', 'I', 0xfb}

func (r *rpc) StoragePoolRefresh(libvirt.StoragePool, uint32) error {
	r.refreshes++
	r.known = map[string]struct{}{}

	entries, err := os.ReadDir(r.target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}

		r.known[entry.Name()] = struct{}{}
	}

	return nil
}

func (r *rpc) StorageVolLookupByName(p libvirt.StoragePool, name string) (libvirt.StorageVol, error) {
	if _, found := r.known[name]; !found {
		return libvirt.StorageVol{}, libvirt.Error{Code: uint32(libvirt.ErrNoStorageVol), Message: "volume not found"}
	}

	return libvirt.StorageVol{Pool: p.Name, Name: name, Key: name}, nil
}

// writeVolumeFile lays down what libvirt would: a sparse file, with a qcow2 header when asked for
// one, so that a refresh can probe its format the way libvirt does.
func writeVolumeFile(path, format string, capacity uint64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}

	defer f.Close() //nolint:errcheck

	if format == "qcow2" {
		if _, err = f.Write(qcow2Magic); err != nil {
			return err
		}
	}

	return f.Truncate(int64(capacity)) //nolint:gosec
}

func (r *rpc) StorageVolCreateXML(p libvirt.StoragePool, text string, flags libvirt.StorageVolCreateFlags) (libvirt.StorageVol, error) {
	var desc libvirtxml.StorageVolume
	if err := desc.Unmarshal(text); err != nil {
		return libvirt.StorageVol{}, err
	}

	if flags != 0 {
		panic("volume creation must pass no flags")
	}

	if desc.Allocation == nil || desc.Allocation.Value != 0 {
		panic("a volume must be created sparse, or the session expires mid-write")
	}

	r.volCalls = append(r.volCalls, "create:"+desc.Name+":"+desc.Target.Format.Type)

	if r.createErr != nil {
		return libvirt.StorageVol{}, r.createErr
	}

	if err := writeVolumeFile(filepath.Join(r.target, desc.Name), desc.Target.Format.Type, desc.Capacity.Value); err != nil {
		return libvirt.StorageVol{}, err
	}

	if r.known == nil {
		r.known = map[string]struct{}{}
	}

	r.known[desc.Name] = struct{}{}

	return libvirt.StorageVol{Pool: p.Name, Name: desc.Name, Key: desc.Name}, nil
}

func (r *rpc) StorageVolGetXMLDesc(vol libvirt.StorageVol, _ uint32) (string, error) {
	contents, err := os.ReadFile(filepath.Join(r.target, vol.Name))
	if err != nil {
		return "", err
	}

	format := "raw"
	if len(contents) >= len(qcow2Magic) && string(contents[:len(qcow2Magic)]) == string(qcow2Magic) {
		format = "qcow2"
	}

	return (&libvirtxml.StorageVolume{
		Name:   vol.Name,
		Target: &libvirtxml.StorageVolumeTarget{Format: &libvirtxml.StorageVolumeTargetFormat{Type: format}},
	}).Marshal()
}

func (r *rpc) StorageVolGetInfo(vol libvirt.StorageVol) (int8, uint64, uint64, error) {
	info, err := os.Stat(filepath.Join(r.target, vol.Name))
	if err != nil {
		return 0, 0, 0, err
	}

	return 0, uint64(info.Size()), uint64(info.Size()), nil //nolint:gosec
}

func (r *rpc) StorageVolGetPath(vol libvirt.StorageVol) (string, error) {
	return filepath.Join(r.target, vol.Name), nil
}

func (r *rpc) StorageVolResize(vol libvirt.StorageVol, capacity uint64, flags libvirt.StorageVolResizeFlags) error {
	if flags&libvirt.StorageVolResizeShrink != 0 {
		panic("a volume must never be shrunk")
	}

	r.volCalls = append(r.volCalls, "resize:"+vol.Name)

	if r.resizeErr != nil {
		return r.resizeErr
	}

	info, err := os.Stat(filepath.Join(r.target, vol.Name))
	if err != nil {
		return err
	}

	if capacity < uint64(info.Size()) { //nolint:gosec
		return libvirt.Error{Code: uint32(libvirt.ErrInvalidArg), Message: "shrinking is not supported"}
	}

	return os.Truncate(filepath.Join(r.target, vol.Name), int64(capacity)) //nolint:gosec
}

// newVolumeFixture builds a client over a pool whose directory really exists, because creation
// renames a staged file into place on the host rather than through libvirt.
func newVolumeFixture(t *testing.T) (*rpc, libvirtstorage.Client, libvirtstorage.Pool) {
	t.Helper()

	id := libvirtstorage.UUID(uuid.MustParse(machine), "images")
	pool := libvirtstorage.Pool{Name: "images", UUID: id}
	r := &rpc{
		pools:  []libvirt.StoragePool{{Name: "images", UUID: libvirt.UUID(id)}},
		target: t.TempDir(),
		known:  map[string]struct{}{},
	}

	return r, libvirtstorage.NewTestClient(r), pool
}

func TestVolumeCreateAndLookup(t *testing.T) {
	t.Parallel()

	rpc, client, pool := newVolumeFixture(t)

	_, found, err := client.Volume(pool, "vm1__data.qcow2")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, 1, rpc.refreshes, "an absent volume must be confirmed against a refreshed pool")

	volume, err := client.CreateVolume(pool, "vm1__data.qcow2", "qcow2", 64<<20)
	require.NoError(t, err)
	assert.Equal(t, "vm1__data.qcow2", volume.Name)
	assert.Equal(t, filepath.Join(rpc.target, "vm1__data.qcow2"), volume.Path)
	assert.Equal(t, "qcow2", volume.Format)
	assert.Equal(t, uint64(64<<20), volume.Capacity)

	// The staged name never survives, and the file is the one that was asked for.
	entries, err := os.ReadDir(rpc.target)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "vm1__data.qcow2", entries[0].Name())

	require.Len(t, rpc.volCalls, 1)
	assert.True(t, strings.HasSuffix(rpc.volCalls[0], ":qcow2"))
	assert.NotEqual(t, "create:vm1__data.qcow2:qcow2", rpc.volCalls[0],
		"a volume must be built under a staged name, so a crash cannot leave a half-written one in place")

	again, found, err := client.Volume(pool, "vm1__data.qcow2")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, volume, again)
}

func TestVolumeCreateRefusesAnExistingName(t *testing.T) {
	t.Parallel()

	_, client, pool := newVolumeFixture(t)

	_, err := client.CreateVolume(pool, "vm1__data.raw", "raw", 1<<20)
	require.NoError(t, err)

	_, err = client.CreateVolume(pool, "vm1__data.raw", "raw", 2<<20)
	require.ErrorContains(t, err, "already exists", "creation must never overwrite a guest's data")
}

// A volume written out of band -- or by this host before its last restart -- is invisible to libvirt
// until the pool is refreshed. It must still be found rather than created over.
func TestVolumeFoundAfterRefresh(t *testing.T) {
	t.Parallel()

	rpc, client, pool := newVolumeFixture(t)

	require.NoError(t, os.WriteFile(filepath.Join(rpc.target, "vm1__data.raw"), make([]byte, 4096), 0o600))

	volume, found, err := client.Volume(pool, "vm1__data.raw")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "raw", volume.Format)
	assert.Equal(t, uint64(4096), volume.Capacity)
	assert.Equal(t, 1, rpc.refreshes)

	_, err = client.CreateVolume(pool, "vm1__data.raw", "raw", 1<<20)
	require.ErrorContains(t, err, "already exists")
}

func TestVolumeResizeGrowsOnly(t *testing.T) {
	t.Parallel()

	_, client, pool := newVolumeFixture(t)

	_, err := client.CreateVolume(pool, "vm1__data.raw", "raw", 1<<20)
	require.NoError(t, err)

	require.NoError(t, client.ResizeVolume(pool, "vm1__data.raw", 2<<20))

	volume, found, err := client.Volume(pool, "vm1__data.raw")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, uint64(2<<20), volume.Capacity)

	// The shrink flag is never passed, so libvirt itself is the last line of defense.
	require.Error(t, client.ResizeVolume(pool, "vm1__data.raw", 1<<20))

	volume, _, err = client.Volume(pool, "vm1__data.raw")
	require.NoError(t, err)
	assert.Equal(t, uint64(2<<20), volume.Capacity, "a refused shrink must leave the volume alone")
}

func TestVolumeOperationsRefuseAForeignPool(t *testing.T) {
	t.Parallel()

	rpc, client, _ := newVolumeFixture(t)
	rpc.pools = []libvirt.StoragePool{{Name: "images", UUID: libvirt.UUID(uuid.New())}}
	pool := libvirtstorage.Pool{Name: "images", UUID: libvirtstorage.UUID(uuid.MustParse(machine), "images")}

	_, _, err := client.Volume(pool, "vm1__data.raw")
	require.ErrorContains(t, err, "not owned")

	_, err = client.CreateVolume(pool, "vm1__data.raw", "raw", 1<<20)
	require.ErrorContains(t, err, "not owned")

	require.ErrorContains(t, client.ResizeVolume(pool, "vm1__data.raw", 1<<20), "not owned")
	assert.Empty(t, rpc.volCalls)
}

// A create interrupted before the rename leaves a staged file. The next create sweeps it, and
// nothing else: without that, a pool directory collects one per interrupted or failed attempt.
func TestCreateVolumeSweepsStagedVolumes(t *testing.T) {
	t.Parallel()

	rpc, client, pool := newVolumeFixture(t)

	_, err := client.CreateVolume(pool, "vm1__data.raw", "raw", 1<<20)
	require.NoError(t, err)

	staged := filepath.Join(rpc.target, ".vm2__data.raw.0123456789abcdef.staged")
	require.NoError(t, os.WriteFile(staged, nil, 0o600))

	_, err = client.CreateVolume(pool, "vm2__data.raw", "raw", 1<<20)
	require.NoError(t, err)

	entries, err := os.ReadDir(rpc.target)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	assert.ElementsMatch(t, []string{"vm1__data.raw", "vm2__data.raw"}, names,
		"a staged leftover must be swept, and a real volume must not be")
}
