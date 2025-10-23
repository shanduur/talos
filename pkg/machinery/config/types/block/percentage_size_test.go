// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos/pkg/machinery/config/types/block"
)

func TestPercentageSizeUnmarshal(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		in   string
		want uint64
	}{
		{in: "", want: 0},
		{in: "100%", want: 100},
		{in: "33.4%", want: 33},
		{in: "33.4124%", want: 33},
	} {
		t.Run(test.in, func(t *testing.T) {
			t.Parallel()

			var ps block.PercentageSize

			require.NoError(t, ps.UnmarshalText([]byte(test.in)))

			assert.Equal(t, test.want, ps.Value())
			assert.Equal(t, test.want, ps.RelativeValue(100))

			out, err := ps.MarshalText()
			require.NoError(t, err)

			assert.Equal(t, test.in, string(out))
		})
	}
}
