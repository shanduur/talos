// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/siderolabs/talos/internal/pkg/libvirt/storage/copyowner"
)

func progressOperation(t *testing.T, data string) *CopyOperation {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.WriteFile(source, []byte(data), 0o600))

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)

	op := &CopyOperation{Source: source, Expected: digest.SHA256.FromString(data).String(), root: root, staged: "stage", release: func() {}}
	t.Cleanup(op.Close)

	return op
}

func TestCopyProgressTransferAndCleanup(t *testing.T) {
	for _, data := range []string{"", strings.Repeat("x", 300000)} {
		t.Run(fmt.Sprintf("bytes=%d", len(data)), func(t *testing.T) {
			op := progressOperation(t, data)

			var events []CopyProgress

			op.SetProgressObserver(func(p CopyProgress) { events = append(events, p) })
			require.NoError(t, op.Transfer(t.Context()))
			require.NotEmpty(t, events, "transfer must emit progress")
			require.Equal(t, CopyPhaseTransfer, events[0].Phase)
			require.True(t, events[0].TotalKnown)
			require.Equal(t, uint64(len(data)), events[0].TotalBytes)

			var boundaries []CopyPhase
			for _, p := range events {
				if len(boundaries) == 0 || boundaries[len(boundaries)-1] != p.Phase {
					boundaries = append(boundaries, p.Phase)
				}

				require.False(t, p.StartedAt.IsZero())
				require.GreaterOrEqual(t, p.Elapsed.Nanoseconds(), int64(0))
			}

			require.Equal(t, []CopyPhase{CopyPhaseTransfer, CopyPhaseVerify, CopyPhaseSync}, boundaries)
			require.Equal(t, uint64(len(data)), events[len(events)-1].BytesCopied)
			op.Close()
			require.Equal(t, CopyPhaseCleanup, events[len(events)-1].Phase)
			count := len(events)

			op.Close()
			require.Len(t, events, count)
		})
	}
}

func TestCopyProgressPartialWrite(t *testing.T) {
	op := &CopyOperation{}

	var events []CopyProgress

	op.SetProgressObserver(func(p CopyProgress) { events = append(events, p) })

	failure := errors.New("disk full")
	n, err := op.write(t.Context(), copyWriteFunc(func([]byte) (int, error) { return 3, failure }), []byte("abcdef"))
	require.Equal(t, 3, n)
	require.ErrorIs(t, err, failure)
	require.NotEmpty(t, events, "partial successful bytes must be observed even on error")
	require.Equal(t, uint64(3), events[0].BytesCopied)
	require.False(t, events[0].TotalKnown)
}

func TestCopyProgressCancellationAndNil(t *testing.T) {
	op := progressOperation(t, strings.Repeat("x", 300000))
	ctx, cancel := context.WithCancel(t.Context())

	var events []CopyProgress

	op.SetProgressObserver(func(p CopyProgress) {
		events = append(events, p)
		if p.BytesCopied > 0 {
			cancel()
		}
	})
	require.ErrorIs(t, op.Transfer(ctx), context.Canceled)
	require.NotEmpty(t, events)

	for _, p := range events {
		require.Equal(t, CopyPhaseTransfer, p.Phase)
	}

	op.Close()
	require.Equal(t, CopyPhaseCleanup, events[len(events)-1].Phase)
	nilOp := progressOperation(t, "nil observer")
	require.NoError(t, nilOp.Transfer(t.Context()))
	require.True(t, nilOp.progress.StartedAt.IsZero())
}

func BenchmarkCopyWriteNilObserver(b *testing.B) {
	op := &CopyOperation{}
	data := make([]byte, 128<<10)
	ctx := context.Background()

	b.ReportAllocs()
	b.SetBytes(int64(len(data)))

	for b.Loop() {
		if _, err := op.write(ctx, io.Discard, data); err != nil {
			b.Fatal(err)
		}
	}
}

func TestCopyProgressSinglePassAndDigestFailure(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			data := strings.Repeat("payload", 50000)

			op := progressOperation(t, data)
			if mismatch {
				op.Expected = digest.SHA256.FromString("different").String()
			}

			input := strings.NewReader(data)

			var readBytes int

			reader := copyReadFunc(func(p []byte) (int, error) {
				n, err := input.Read(p)
				readBytes += n

				return n, err
			})

			var phases []CopyPhase

			op.SetProgressObserver(func(p CopyProgress) { phases = append(phases, p.Phase) })
			err := op.writeCopyContext(t.Context(), reader)
			require.Equal(t, len(data), readBytes)
			require.Zero(t, input.Len())
			require.Equal(t, uint64(len(data)), op.progress.BytesCopied)
			require.False(t, op.progress.TotalKnown)

			if mismatch {
				require.ErrorContains(t, err, "digest mismatch")
				require.Equal(t, CopyPhaseVerify, phases[len(phases)-1])
				require.NotContains(t, phases, CopyPhaseSync)
			} else {
				require.NoError(t, err)
				require.Equal(t, CopyPhaseSync, phases[len(phases)-1])
			}
		})
	}
}

func TestCopyProgressNilObserverAllocations(t *testing.T) {
	op := &CopyOperation{}
	data := make([]byte, 128<<10)
	ctx := context.Background()

	require.Zero(t, testing.AllocsPerRun(100, func() {
		_, err := op.write(ctx, io.Discard, data)
		if err != nil {
			panic(err)
		}
	}))
	require.True(t, op.progress.StartedAt.IsZero())
}

func TestCopyBindingKeyRejectsInvalidAndLateAssignment(t *testing.T) {
	root, err := os.OpenRoot(privateFixture(t))
	require.NoError(t, err)

	defer root.Close() //nolint:errcheck

	op := &CopyOperation{root: root}
	require.ErrorContains(t, op.SetBindingKey("bad"), "invalid copy binding key")

	key := strings.Repeat("a", 64)
	require.NoError(t, op.SetBindingKey(key))
	require.Equal(t, key, op.bindingKey)
	op.transferred = true
	require.ErrorContains(t, op.SetBindingKey(strings.Repeat("b", 64)), "before transfer")
	require.Equal(t, key, op.bindingKey)
}

func TestConcurrentCopyPublicationKeepsWinningOwner(t *testing.T) {
	target := privateFixture(t)
	root, err := os.OpenRoot(target)
	require.NoError(t, err)

	defer root.Close() //nolint:errcheck

	name, key := "concurrent.iso", strings.Repeat("a", 64)
	start := make(chan struct{})
	results := make(chan error, 2)

	for _, nonce := range []string{"0000000000000001", "0000000000000002"} {
		op := &CopyOperation{
			root:       root,
			Name:       name,
			bindingKey: key,
			staged:     ".copy-on-use/.concurrent.iso." + nonce + ".staged",
			witness:    copyowner.Witness(name, false),
		}
		require.NoError(t, os.WriteFile(filepath.Join(target, op.staged), []byte(nonce), 0o600))

		go func() {
			<-start

			results <- op.publish(t.Context(), func() error { return nil })
		}()
	}

	close(start)

	successes := 0

	for range 2 {
		if <-results == nil {
			successes++
		}
	}

	require.Equal(t, 1, successes)

	info, err := root.Lstat(name)
	require.NoError(t, err)
	require.True(t, copyowner.OwnerMatches(root, key, name, info))
}

func TestCopyPublicationSerializesFinalValidation(t *testing.T) {
	target := privateFixture(t)
	root, err := os.OpenRoot(target)
	require.NoError(t, err)

	defer root.Close() //nolint:errcheck

	op := &CopyOperation{
		root:       root,
		Name:       "serial.iso",
		bindingKey: strings.Repeat("a", 64),
		staged:     ".copy-on-use/.serial.iso.0000000000000000.staged",
		witness:    copyowner.Witness("serial.iso", false),
	}
	require.NoError(t, os.WriteFile(filepath.Join(target, op.staged), []byte("payload"), 0o600))
	require.NoError(t, op.publish(t.Context(), func() error {
		unlocked := copyPublications.TryLock()
		if unlocked {
			copyPublications.Unlock()
		}

		require.False(t, unlocked, "final validation must run inside publication serialization")

		return nil
	}))
	info, err := root.Lstat(op.Name)
	require.NoError(t, err)
	require.True(t, copyowner.OwnerMatches(root, op.bindingKey, op.Name, info))
}

func TestLongCopyOwnerPublicationRecoveryAndCleanup(t *testing.T) {
	target := privateFixture(t)
	root, err := os.OpenRoot(target)
	require.NoError(t, err)

	defer root.Close() //nolint:errcheck

	name, key := strings.Repeat("n", 251)+".iso", strings.Repeat("a", 64)
	op := &CopyOperation{
		root:       root,
		Name:       name,
		bindingKey: key,
		staged:     ".copy-on-use/long-stage",
		witness:    copyowner.Witness(name, false),
	}
	require.NoError(t, os.WriteFile(filepath.Join(target, op.staged), []byte("long-name payload"), 0o600))
	require.NoError(t, op.publish(t.Context(), func() error { return nil }))

	info, err := root.Lstat(name)
	require.NoError(t, err)
	require.True(t, copyowner.OwnerMatches(root, key, name, info))
	require.NoError(t, os.Remove(filepath.Join(target, name)))
	require.NoError(t, copyowner.Collect(context.Background(), target, copyowner.Retention{Bindings: map[string]struct{}{key: {}}}))
	require.FileExists(t, filepath.Join(target, op.witness), "wanted copy must remain recoverable")
	recovered, err := copyowner.Recover(root, name, op.witness)
	require.NoError(t, err)
	require.True(t, recovered)
	require.FileExists(t, filepath.Join(target, name))
	require.NoError(t, os.Remove(filepath.Join(target, name)))
	require.NoError(t, copyowner.Collect(context.Background(), target, copyowner.Retention{}))
	require.NoFileExists(t, filepath.Join(target, op.witness))
	require.NoFileExists(t, filepath.Join(target, copyowner.OwnerPath(name)))
}

func TestOwnedRawGrowthRejectsFileSizeLimit(t *testing.T) {
	if os.Getenv("TALOS_RAW_LIMIT_TEST_CHILD") != "1" {
		child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestOwnedRawGrowthRejectsFileSizeLimit$")

		child.Env = append(os.Environ(), "TALOS_RAW_LIMIT_TEST_CHILD=1")
		output, err := child.CombinedOutput()
		require.NoError(t, err, "%s", output)

		return
	}

	// Limit only this disposable test child, never the parent process or host.
	signal.Ignore(syscall.SIGXFSZ)

	target := t.TempDir()
	name := "guest.raw"
	require.NoError(t, os.WriteFile(filepath.Join(target, name), []byte("data"), 0o600))
	root, err := os.OpenRoot(target)
	require.NoError(t, err)

	defer root.Close() //nolint:errcheck

	owner, err := copyowner.ClaimImageOwner(root, name)
	require.NoError(t, err)

	identity := copyowner.ImageMarker("raw", owner)

	var limit unix.Rlimit

	require.NoError(t, unix.Getrlimit(unix.RLIMIT_FSIZE, &limit))
	require.NoError(t, unix.Setrlimit(unix.RLIMIT_FSIZE, &unix.Rlimit{Cur: 8, Max: limit.Max}))

	defer func() { require.NoError(t, unix.Setrlimit(unix.RLIMIT_FSIZE, &limit)) }()

	outcome, err := growPinnedRawImage(root, name, identity, 16)
	require.ErrorIs(t, err, syscall.EFBIG)
	require.Equal(t, Finished, outcome, "a definitive local rejection must not leave an unknown-mutation fence")

	info, err := root.Stat(name)
	require.NoError(t, err)
	require.EqualValues(t, 4, info.Size())

	outcome, err = growPinnedRawImage(root, name, identity, 8)
	require.NoError(t, err)
	require.Equal(t, Finished, outcome)

	info, err = root.Stat(name)
	require.NoError(t, err)
	require.EqualValues(t, 8, info.Size())

	data, err := os.ReadFile(filepath.Join(target, name))
	require.NoError(t, err)
	require.Equal(t, "data", string(data[:4]))
}

func privateFixture(t *testing.T) string {
	t.Helper()

	target := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(target, copyowner.Dir), 0o700))

	return target
}
