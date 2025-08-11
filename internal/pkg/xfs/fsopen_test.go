// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build unix

package xfs_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/internal/pkg/xfs"
	"github.com/siderolabs/talos/internal/pkg/xfs/fsopen"
)

func TestTmpfs(t *testing.T) {
	t.Parallel()

	if uid := os.Getuid(); uid != 0 {
		t.Skipf("skipping test, not running as root (uid %d)", uid)
	}

	for _, tc := range []struct {
		fstype string
		opts   []fsopen.Option
	}{
		{fstype: "tmpfs"},
	} {
		t.Run(tc.fstype, func(t *testing.T) {
			t.Parallel()

			fsc, err := fsopen.New(tc.fstype, tc.opts...)
			require.NoError(t, err)

			fs, err := xfs.NewUnix(fsc)
			require.NoError(t, err)

			t.Cleanup(func() {
				err := fsc.Close()
				require.NoError(t, err)
			})

			testFilesystem(t, fs, nil)
		})
	}
}
