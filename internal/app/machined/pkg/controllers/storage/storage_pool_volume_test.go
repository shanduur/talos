// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	storagectrl "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/storage"
	libvirtstorage "github.com/siderolabs/talos/internal/pkg/libvirt/storage"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"
	"github.com/siderolabs/talos/pkg/machinery/resources/hypervisor"
	storageres "github.com/siderolabs/talos/pkg/machinery/resources/storage"
)

const (
	volumeFinalizer = "storage.StoragePoolVolumeController"

	// volumePool is the one pool every case in this suite uses. Named rather than passed: a pool
	// name threaded through every helper reads like a dimension these tests vary, and they do not.
	volumePool = "images"
)

// volumeClient records what the controller asks of the storage daemon, and refuses anything it asks
// without first holding the pool it is asking about.
type volumeClient struct {
	mu        sync.Mutex
	volumes   map[string]libvirtstorage.Volume
	calls     []string
	openErr   error
	createErr error
	resizeErr error
	opens     int
	checkHold func(pool string) error
}

func (c *volumeClient) open(context.Context) (libvirtstorage.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.opens++

	if c.openErr != nil {
		return nil, c.openErr
	}

	return c, nil
}

func (c *volumeClient) key(pool libvirtstorage.Pool, name string) string {
	return pool.Name + "/" + name
}

func (c *volumeClient) Volume(pool libvirtstorage.Pool, name string) (libvirtstorage.Volume, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.checkHold(pool.Name); err != nil {
		return libvirtstorage.Volume{}, false, err
	}

	volume, found := c.volumes[c.key(pool, name)]

	return volume, found, nil
}

func (c *volumeClient) CreateVolume(pool libvirtstorage.Pool, name, format string, capacity uint64) (libvirtstorage.Volume, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.checkHold(pool.Name); err != nil {
		return libvirtstorage.Volume{}, err
	}

	c.calls = append(c.calls, "create:"+c.key(pool, name))

	if c.createErr != nil {
		return libvirtstorage.Volume{}, c.createErr
	}

	volume := libvirtstorage.Volume{
		Name:     name,
		Path:     "/var/mnt/u-vms/" + pool.Name + "/" + name,
		Format:   format,
		Capacity: capacity,
	}

	if c.volumes == nil {
		c.volumes = map[string]libvirtstorage.Volume{}
	}

	c.volumes[c.key(pool, name)] = volume

	return volume, nil
}

func (c *volumeClient) ResizeVolume(pool libvirtstorage.Pool, name string, capacity uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.checkHold(pool.Name); err != nil {
		return err
	}

	c.calls = append(c.calls, "resize:"+c.key(pool, name))

	if c.resizeErr != nil {
		return c.resizeErr
	}

	volume := c.volumes[c.key(pool, name)]
	if capacity < volume.Capacity {
		return errors.New("a volume must never be shrunk")
	}

	volume.Capacity = capacity
	c.volumes[c.key(pool, name)] = volume

	return nil
}

func (*volumeClient) Pools() ([]libvirtstorage.Pool, error) {
	panic("StoragePoolVolumeController must not reconcile pool definitions")
}

func (*volumeClient) Ensure(libvirtstorage.Pool, string, func() error) error {
	panic("StoragePoolVolumeController must not reconcile pool definitions")
}

func (*volumeClient) Remove(libvirtstorage.Pool) error {
	panic("StoragePoolVolumeController must not reconcile pool definitions")
}

func (*volumeClient) Stop(libvirtstorage.Pool) error {
	panic("StoragePoolVolumeController must not reconcile pool definitions")
}

func (*volumeClient) Close() {}

func (c *volumeClient) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.calls)
}

func (c *volumeClient) volume(pool, name string) (libvirtstorage.Volume, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	volume, found := c.volumes[pool+"/"+name]

	return volume, found
}

func (c *volumeClient) setVolume(name string, volume libvirtstorage.Volume) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.volumes == nil {
		c.volumes = map[string]libvirtstorage.Volume{}
	}

	c.volumes[volumePool+"/"+name] = volume
}

func (c *volumeClient) setErrors(openErr, createErr, resizeErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.openErr, c.createErr, c.resizeErr = openErr, createErr, resizeErr
}

type StoragePoolVolumeSuite struct {
	ctest.DefaultSuite

	client *volumeClient
	logs   *observer.ObservedLogs
}

func (suite *StoragePoolVolumeSuite) SetupTest() {
	// Teed onto the suite's own logger so that "controller failed" -- which the runtime logs when
	// Run returns -- is observable. It is the only crisp signal of a restart; counting reconcile
	// passes is not, since the controller writes one of its own inputs.
	var observed zapcore.Core

	observed, suite.logs = observer.New(zapcore.ErrorLevel)
	suite.Logger = zap.New(zapcore.NewTee(zaptest.NewLogger(suite.T()).Core(), observed))

	suite.DefaultSuite.SetupTest()
	suite.client = &volumeClient{checkHold: suite.checkHold}

	system := hardware.NewSystemInformation(hardware.SystemInformationID)
	system.TypedSpec().UUID = poolMachineUUID
	suite.Create(system)
}

// checkHold is the invariant the whole design rests on: nothing is done to a pool's contents before
// that pool is held, or it could be stopped and unmounted mid-operation.
func (suite *StoragePoolVolumeSuite) checkHold(pool string) error {
	status, err := safe.StateGetByID[*storageres.StoragePoolStatus](suite.Ctx(), suite.State(), pool)
	if err != nil {
		return err
	}

	if !status.Metadata().Finalizers().Has(volumeFinalizer) {
		return fmt.Errorf("volume operation in pool %q without holding it", pool)
	}

	return nil
}

func (suite *StoragePoolVolumeSuite) start() {
	suite.Require().NoError(suite.Runtime().RegisterController(&storagectrl.StoragePoolVolumeController{Open: suite.client.open}))
}

func (suite *StoragePoolVolumeSuite) pool(ready bool) *storageres.StoragePoolStatus {
	status := storageres.NewStoragePoolStatus(storageres.NamespaceName, volumePool)
	status.TypedSpec().VolumeID = "u-vms"
	status.TypedSpec().TargetPath = "/var/mnt/u-vms/" + volumePool
	status.TypedSpec().Ready = ready

	if !ready {
		status.TypedSpec().Error = "waiting for backing volume"
	}

	suite.Create(status, state.WithCreateOwner("storage.StoragePoolController"))

	return status
}

func (suite *StoragePoolVolumeSuite) spec(pool, name, format string, capacity uint64) *storageres.StoragePoolVolumeSpec {
	spec := storageres.NewStoragePoolVolumeSpec(storageres.NamespaceName, storageres.StoragePoolVolumeID(pool, name))
	spec.TypedSpec().Pool = pool
	spec.TypedSpec().Name = name
	spec.TypedSpec().Format = format
	spec.TypedSpec().Capacity = capacity
	suite.Create(spec, state.WithCreateOwner("hypervisor.VirtualMachineDiskController"))

	return spec
}

// openDisk models a running domain: a disk status naming the volume, with a finalizer on it.
func (suite *StoragePoolVolumeSuite) openDisk(pool, volume string) *hypervisor.VirtualMachineDiskStatus {
	status := hypervisor.NewVirtualMachineDiskStatus(hypervisor.NamespaceName, "vm1/data@0123456789ab")
	status.TypedSpec().VirtualMachine = "vm1"
	status.TypedSpec().Name = "data"
	status.TypedSpec().Pool = pool
	status.TypedSpec().Volume = volume
	status.TypedSpec().Blank = true
	suite.Create(status, state.WithCreateOwner("hypervisor.VirtualMachineDiskController"))

	suite.Require().NoError(suite.State().AddFinalizer(suite.Ctx(), status.Metadata(), "hypervisor.VirtualMachineController"))

	return status
}

func (suite *StoragePoolVolumeSuite) assertReady(id string, check func(*storageres.StoragePoolVolumeStatusSpec, *assert.Assertions)) {
	ctest.AssertResource(suite, id, func(status *storageres.StoragePoolVolumeStatus, asrt *assert.Assertions) {
		asrt.True(status.TypedSpec().Ready, status.TypedSpec().Error)

		if check != nil {
			check(status.TypedSpec(), asrt)
		}
	})
}

func (suite *StoragePoolVolumeSuite) assertError(id, reason string) {
	ctest.AssertResource(suite, id, func(status *storageres.StoragePoolVolumeStatus, asrt *assert.Assertions) {
		asrt.Contains(status.TypedSpec().Error, reason)
	})
}

func (suite *StoragePoolVolumeSuite) assertPoolHold(held bool) {
	ctest.AssertResource(suite, volumePool, func(status *storageres.StoragePoolStatus, asrt *assert.Assertions) {
		asrt.Equal(held, status.Metadata().Finalizers().Has(volumeFinalizer))
	})
}

// assertNoRestartLoop fails if the controller returned from Run. A condition reported through the
// volume's status must not also be returned: that restarts the controller under backoff for as long
// as the condition lasts, which for a format mismatch or an oversized volume is forever.
func (suite *StoragePoolVolumeSuite) assertNoRestartLoop() {
	suite.Require().Never(func() bool { return suite.logs.FilterMessage("controller failed").Len() > 0 },
		500*time.Millisecond, 50*time.Millisecond, "a reported condition must not restart the controller")
}

// tearDownPool starts a pool's teardown, keeping its status observable through another consumer's
// hold the way StoragePoolController's real consumers do.
func (suite *StoragePoolVolumeSuite) tearDownPool(pool *storageres.StoragePoolStatus) {
	suite.Require().NoError(suite.State().AddFinalizer(suite.Ctx(), pool.Metadata(), "test.OtherConsumer"))

	_, err := suite.State().Teardown(suite.Ctx(), pool.Metadata(), state.WithTeardownOwner("storage.StoragePoolController"))
	suite.Require().NoError(err)
}

func TestStoragePoolVolumeSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, &StoragePoolVolumeSuite{
		Timeout: 10 * time.Second,
	})
}

// withdrawSpec models its author giving the volume up: torn down first, and destroyed only once the
// hold on it comes back.
func (suite *StoragePoolVolumeSuite) withdrawSpec(spec *storageres.StoragePoolVolumeSpec) {
	_, err := suite.State().Teardown(suite.Ctx(), spec.Metadata(), state.WithTeardownOwner("hypervisor.VirtualMachineDiskController"))
	suite.Require().NoError(err)

	ctest.AssertResource(suite, spec.Metadata().ID(), func(spec *storageres.StoragePoolVolumeSpec, asrt *assert.Assertions) {
		asrt.False(spec.Metadata().Finalizers().Has(volumeFinalizer))
	})

	suite.Require().NoError(suite.State().Destroy(suite.Ctx(), spec.Metadata(), state.WithDestroyOwner("hypervisor.VirtualMachineDiskController")))
}

func (suite *StoragePoolVolumeSuite) TestCreatesWhenAbsent() {
	suite.pool(true)
	suite.spec("images", "vm1__data.qcow2", "qcow2", 64<<20)
	suite.start()

	id := storageres.StoragePoolVolumeID("images", "vm1__data.qcow2")
	suite.assertReady(id, func(spec *storageres.StoragePoolVolumeStatusSpec, asrt *assert.Assertions) {
		asrt.Equal("/var/mnt/u-vms/images/vm1__data.qcow2", spec.Path)
		asrt.Equal("qcow2", spec.Format)
		asrt.Equal(uint64(64<<20), spec.Capacity)
		asrt.Zero(spec.PendingCapacity)
		asrt.Empty(spec.Error)
	})

	suite.assertPoolHold(true)
	suite.Require().Equal([]string{"create:images/vm1__data.qcow2"}, suite.client.recorded())
}

// The ordinary case after any reboot: the volume is already there, and the same names mean the same
// disk. Adoption must cost no daemon call at all.
func (suite *StoragePoolVolumeSuite) TestAdoptsAnExistingVolume() {
	suite.pool(true)
	suite.client.setVolume("vm1__data.qcow2", libvirtstorage.Volume{
		Name: "vm1__data.qcow2", Path: "/var/mnt/u-vms/images/vm1__data.qcow2", Format: "qcow2", Capacity: 64 << 20,
	})
	suite.spec("images", "vm1__data.qcow2", "qcow2", 64<<20)
	suite.start()

	suite.assertReady(storageres.StoragePoolVolumeID("images", "vm1__data.qcow2"), nil)
	suite.Require().Empty(suite.client.recorded(), "an adopted volume must be neither created nor resized")
}

func (suite *StoragePoolVolumeSuite) TestGrowsAVolumeNothingHasOpen() {
	suite.pool(true)
	suite.client.setVolume("vm1__data.qcow2", libvirtstorage.Volume{
		Name: "vm1__data.qcow2", Path: "/var/mnt/u-vms/images/vm1__data.qcow2", Format: "qcow2", Capacity: 64 << 20,
	})
	suite.spec("images", "vm1__data.qcow2", "qcow2", 128<<20)
	suite.start()

	suite.assertReady(storageres.StoragePoolVolumeID("images", "vm1__data.qcow2"),
		func(spec *storageres.StoragePoolVolumeStatusSpec, asrt *assert.Assertions) {
			asrt.Equal(uint64(128<<20), spec.Capacity)
			asrt.Zero(spec.PendingCapacity)
			// A ready status with no path renders as <source file=''/>.
			asrt.Equal("/var/mnt/u-vms/images/vm1__data.qcow2", spec.Path)
			asrt.Equal("qcow2", spec.Format)
		})

	suite.Require().Equal([]string{"resize:images/vm1__data.qcow2"}, suite.client.recorded())
}

// Growing a volume QEMU has open corrupts qcow2 metadata and silently lies for raw. The growth is
// remembered instead, and the disk stays attachable so the guest is not stopped over it.
func (suite *StoragePoolVolumeSuite) TestDefersAGrowthWhileTheVolumeIsOpen() {
	suite.pool(true)
	suite.client.setVolume("vm1__data.qcow2", libvirtstorage.Volume{
		Name: "vm1__data.qcow2", Path: "/var/mnt/u-vms/images/vm1__data.qcow2", Format: "qcow2", Capacity: 64 << 20,
	})
	suite.openDisk("images", "vm1__data.qcow2")
	suite.spec("images", "vm1__data.qcow2", "qcow2", 128<<20)
	suite.start()

	id := storageres.StoragePoolVolumeID("images", "vm1__data.qcow2")
	suite.assertReady(id, func(spec *storageres.StoragePoolVolumeStatusSpec, asrt *assert.Assertions) {
		asrt.Equal(uint64(64<<20), spec.Capacity)
		asrt.Equal(uint64(128<<20), spec.PendingCapacity)
	})
	suite.assertError(id, "stop the virtual machine to apply it")

	suite.Require().Empty(suite.client.recorded(), "a volume a guest has open must not be resized")

	volume, _ := suite.client.volume("images", "vm1__data.qcow2")
	suite.Require().Equal(uint64(64<<20), volume.Capacity)

	suite.assertNoRestartLoop()
}

// A smaller size is reported and ignored. Refusing the disk outright would stop a running guest
// over an edited number, which is worse than the mismatch.
func (suite *StoragePoolVolumeSuite) TestRefusesToShrinkButStaysReady() {
	suite.pool(true)
	suite.client.setVolume("vm1__data.qcow2", libvirtstorage.Volume{
		Name: "vm1__data.qcow2", Path: "/var/mnt/u-vms/images/vm1__data.qcow2", Format: "qcow2", Capacity: 128 << 20,
	})
	suite.spec("images", "vm1__data.qcow2", "qcow2", 64<<20)
	suite.start()

	id := storageres.StoragePoolVolumeID("images", "vm1__data.qcow2")
	suite.assertReady(id, func(spec *storageres.StoragePoolVolumeStatusSpec, asrt *assert.Assertions) {
		asrt.Equal(uint64(128<<20), spec.Capacity)
	})
	suite.assertError(id, "never shrunk")
	suite.Require().Empty(suite.client.recorded())
	suite.assertNoRestartLoop()
}

// A format mismatch is the one case that does withhold the disk: attaching a qcow2 file as raw
// hands the guest its header as block zero.
func (suite *StoragePoolVolumeSuite) TestRefusesAFormatMismatch() {
	suite.pool(true)
	suite.client.setVolume("vm1__data.raw", libvirtstorage.Volume{
		Name: "vm1__data.raw", Path: "/var/mnt/u-vms/images/vm1__data.raw", Format: "qcow2", Capacity: 64 << 20,
	})
	suite.spec("images", "vm1__data.raw", "raw", 64<<20)
	suite.start()

	id := storageres.StoragePoolVolumeID("images", "vm1__data.raw")
	ctest.AssertResource(suite, id, func(status *storageres.StoragePoolVolumeStatus, asrt *assert.Assertions) {
		asrt.False(status.TypedSpec().Ready)
		asrt.Contains(status.TypedSpec().Error, "move the file aside")
	})
	suite.Require().Empty(suite.client.recorded(), "a volume of the wrong format must be neither rewritten nor deleted")
	suite.assertNoRestartLoop()
}

func (suite *StoragePoolVolumeSuite) TestWaitsForThePool() {
	suite.pool(false)
	suite.spec("images", "vm1__data.qcow2", "qcow2", 64<<20)
	suite.start()

	suite.assertError(storageres.StoragePoolVolumeID("images", "vm1__data.qcow2"), "is not ready")
	suite.Require().Empty(suite.client.recorded())
}

func (suite *StoragePoolVolumeSuite) TestWaitsForAnUndeclaredPool() {
	suite.spec("nowhere", "vm1__data.qcow2", "qcow2", 64<<20)
	suite.start()

	suite.assertError(storageres.StoragePoolVolumeID("nowhere", "vm1__data.qcow2"), "not declared by a StoragePool document")
	suite.Require().Zero(suite.client.opens, "a volume with no pool must not dial the storage daemon")
}

// Withdrawing the spec withdraws the status and the hold -- and nothing else. There is no verb on
// the storage client which could delete the volume, and there must not be.
func (suite *StoragePoolVolumeSuite) TestSpecRemovalRetainsTheVolume() {
	suite.pool(true)
	spec := suite.spec("images", "vm1__data.qcow2", "qcow2", 64<<20)
	suite.start()

	id := storageres.StoragePoolVolumeID("images", "vm1__data.qcow2")
	suite.assertReady(id, nil)
	suite.assertPoolHold(true)

	suite.withdrawSpec(spec)

	ctest.AssertNoResource[*storageres.StoragePoolVolumeStatus](suite, id)
	suite.assertPoolHold(false)

	_, found := suite.client.volume("images", "vm1__data.qcow2")
	suite.Require().True(found, "removing the configuration must never delete a volume")
	suite.Require().Equal([]string{"create:images/vm1__data.qcow2"}, suite.client.recorded())
}

// The pool is held once for all its volumes, and released only when the last one goes.
func (suite *StoragePoolVolumeSuite) TestPoolIsHeldUntilTheLastVolumeGoes() {
	suite.pool(true)
	first := suite.spec("images", "vm1__data.qcow2", "qcow2", 64<<20)
	suite.spec("images", "vm2__data.qcow2", "qcow2", 64<<20)
	suite.start()

	suite.assertReady(storageres.StoragePoolVolumeID("images", "vm1__data.qcow2"), nil)
	suite.assertReady(storageres.StoragePoolVolumeID("images", "vm2__data.qcow2"), nil)

	suite.withdrawSpec(first)

	ctest.AssertNoResource[*storageres.StoragePoolVolumeStatus](suite, storageres.StoragePoolVolumeID("images", "vm1__data.qcow2"))
	suite.assertPoolHold(true)
}

// A pool on its way out is refused rather than held: taking a hold now would block its teardown for
// good.
func (suite *StoragePoolVolumeSuite) TestRefusesToHoldAPoolGoingAway() {
	pool := suite.pool(true)
	suite.spec("images", "vm1__data.qcow2", "qcow2", 64<<20)
	suite.start()

	id := storageres.StoragePoolVolumeID("images", "vm1__data.qcow2")
	suite.assertReady(id, nil)

	suite.tearDownPool(pool)

	suite.assertError(id, "is going away")
	suite.assertPoolHold(false)
}

// The same pool, but with a guest still writing into it. Here the hold must stay: releasing it lets
// StoragePoolController remove the pool and give the mount back while QEMU has the qcow2 open by
// file descriptor. A volume status cannot say so -- nothing finalizes one -- so the disk status a
// running domain holds is what keeps the pool in place.
func (suite *StoragePoolVolumeSuite) TestKeepsAPoolGoingAwayHeldForAGuest() {
	pool := suite.pool(true)
	suite.client.setVolume("vm1__data.qcow2", libvirtstorage.Volume{
		Name: "vm1__data.qcow2", Path: "/var/mnt/u-vms/images/vm1__data.qcow2", Format: "qcow2", Capacity: 64 << 20,
	})

	spec := suite.spec("images", "vm1__data.qcow2", "qcow2", 64<<20)
	suite.start()

	id := storageres.StoragePoolVolumeID("images", "vm1__data.qcow2")
	suite.assertReady(id, nil)
	suite.assertPoolHold(true)

	disk := suite.openDisk("images", "vm1__data.qcow2")

	// The configuration drops both the pool and the disk, which is what a guest outliving its own
	// spec looks like from here.
	suite.tearDownPool(pool)
	suite.withdrawSpec(spec)

	ctest.AssertNoResource[*storageres.StoragePoolVolumeStatus](suite, id)

	suite.Require().Never(func() bool {
		status, err := safe.StateGetByID[*storageres.StoragePoolStatus](suite.Ctx(), suite.State(), "images")

		return err == nil && !status.Metadata().Finalizers().Has(volumeFinalizer)
	}, time.Second, 50*time.Millisecond, "a pool a guest is writing into must stay held")

	// It does come back once the domain lets the disk go.
	suite.Require().NoError(suite.State().RemoveFinalizer(suite.Ctx(), disk.Metadata(), "hypervisor.VirtualMachineController"))
	suite.assertPoolHold(false)
}

func (suite *StoragePoolVolumeSuite) TestReportsAnUnavailableDaemon() {
	suite.client.setErrors(errors.New("dial unix: no such file"), nil, nil)
	suite.pool(true)
	suite.spec("images", "vm1__data.qcow2", "qcow2", 64<<20)
	suite.start()

	suite.assertError(storageres.StoragePoolVolumeID("images", "vm1__data.qcow2"), "waiting for storage daemon")
}
