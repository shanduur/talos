// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package copyowner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/siderolabs/talos/internal/pkg/qemuimg"
)

func TestRemoveSamePrivateRejectsReplacedWitness(t *testing.T) {
	target := cleanupFixture(t)
	name, key := "replaced-during-retirement.iso", strings.Repeat("a", 64)
	witness := Witness(name, false)
	path := filepath.Join(target, witness)
	require.NoError(t, os.WriteFile(path, []byte("owned payload"), 0o600))
	owner := writeCleanupOwner(t, target, key, name)
	info, err := os.Lstat(path)
	require.NoError(t, err)
	require.NoError(t, os.Link(path, filepath.Join(target, ".pin-original-inode")))
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, []byte("foreign replacement"), 0o600))

	root, err := os.OpenRoot(target)
	require.NoError(t, err)

	defer root.Close() //nolint:errcheck

	require.ErrorContains(t, removeSamePrivate(root, witness, info), "changed during cleanup",
		"retirement must not mistake an identity mismatch for a successful payload unlink")
	require.FileExists(t, owner)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "foreign replacement", string(data))
}

func TestImageOwnerReadErrorClassification(t *testing.T) {
	for _, unsupported := range []error{unix.ENOTSUP, unix.EOPNOTSUPP, unix.ENOSYS} {
		err := classifyImageOwnerReadError(fmt.Errorf("read image ownership: %w", unsupported))

		var validation *qemuimg.ValidationError

		require.ErrorAs(t, err, &validation)
		require.ErrorIs(t, err, unsupported)
	}

	other := fmt.Errorf("read image ownership: %w", unix.EIO)
	err := classifyImageOwnerReadError(other)
	require.True(t, errors.Is(err, unix.EIO))

	var validation *qemuimg.ValidationError

	require.False(t, errors.As(err, &validation))
}

func TestLongCopyOwnerRejectsMalformedRecord(t *testing.T) {
	target := cleanupFixture(t)
	root, err := os.OpenRoot(target)
	require.NoError(t, err)

	defer root.Close() //nolint:errcheck

	name := strings.Repeat("n", 255)
	path := OwnerPath(name)
	require.NoError(t, os.WriteFile(filepath.Join(target, path), []byte(strings.Repeat("a", 64)+"\n"+name+"\n1:2\nEXTRA"), 0o600))
	key, ownerName, info, valid, err := readCopyOwner(root, path)
	require.NoError(t, err)
	require.False(t, valid)
	require.Empty(t, key)
	require.Empty(t, ownerName)
	require.NotNil(t, info)
}

func cleanupFixture(t *testing.T) string {
	t.Helper()

	target := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(target, Dir), 0o700))

	return target
}

func writeCleanupOwner(t *testing.T, target, key, name string) string {
	t.Helper()

	info, err := os.Lstat(filepath.Join(target, Witness(name, false)))
	require.NoError(t, err)

	path := filepath.Join(target, OwnerPath(name))
	require.NoError(t, os.WriteFile(path, []byte(key+"\n"+name+"\n"+copyInode(info)+"\n"), 0o600))

	return path
}
