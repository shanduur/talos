// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package qemuimg provides bounded runtime operations on independent disk images.
package qemuimg

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"syscall"
)

// Binary is supplied by the qemu system extension in machined's mount namespace.
const Binary = "/usr/local/bin/qemu-img"

const (
	rawFormat   = "raw"
	qcow2Format = "qcow2"
)

// Runner executes binary with args to completion. files become descriptors
// 3, 4, ... of the child. A non-zero exit is reported through code with a nil
// error; err is reserved for failures to run or wait for the child.
type Runner func(ctx context.Context, binary string, args []string, files []*os.File, stdout, stderr io.Writer) (code int, err error)

// Client executes qemu-img. Files stay open until the child has exited.
type Client struct {
	binary string
	runner Runner
}

// New selects the executable, normally Binary. An empty path selects Binary.
// The child runs as a direct subprocess until WithRunner selects another runner.
func New(path string) *Client {
	if path == "" {
		path = Binary
	}

	return &Client{binary: path, runner: runDirect}
}

// WithRunner returns a client running qemu-img through runner, e.g. in a sandbox.
func (c *Client) WithRunner(runner Runner) *Client {
	return &Client{binary: c.binary, runner: runner}
}

func runDirect(ctx context.Context, binary string, args []string, files []*os.File, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.ExtraFiles = files
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.Exited() {
		return exitErr.ExitCode(), nil
	}

	if err != nil {
		return -1, toolStartError(err)
	}

	return 0, nil
}

// Info describes the guest-visible image and its external dependencies.
type Info struct {
	Format         string `json:"format"`
	VirtualSize    uint64 `json:"virtual-size"`
	BackingFile    string `json:"backing-filename"`
	FormatSpecific struct {
		Data struct {
			DataFile string `json:"data-file"`
		} `json:"data"`
	} `json:"format-specific"`
}

// ConvertSpec requests an independent image at a required virtual capacity.
// Destination must be a private, disposable regular file, never a published disk.
type ConvertSpec struct {
	Source      *os.File
	Destination *os.File
	Format      string
	Capacity    uint64
	// Result receives verified destination metadata after resize.
	Result *Info
	// Progress receives qemu-img's conversion percentage on the worker goroutine.
	Progress func(float64)
}

// Inspect validates metadata before QEMU can open image-controlled references.
// Raw is always opened explicitly as raw. For qcow2, only versions 2 and 3
// without encryption, backing files or unsupported incompatible features are accepted.
func Inspect(file *os.File) (string, error) {
	stat, err := file.Stat()
	if err != nil {
		return "", err
	}

	if !stat.Mode().IsRegular() {
		return "", invalid("image is not a regular file")
	}

	var header [104]byte

	n, err := file.ReadAt(header[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}

	if n < 4 || string(header[:4]) != "QFI\xfb" {
		if kind := knownNonRaw(header[:n]); kind != "" {
			return "", invalid("source is a %s image; only uncompressed raw or qcow2 images are supported", kind)
		}

		return rawFormat, nil
	}

	return inspectQCOW2(header[:n])
}

// knownNonRaw catches common sources that would otherwise be byte-copied as raw.
// Raw has no signature, so this is a best-effort guard, not a format probe.
func knownNonRaw(header []byte) string {
	prefixes := []struct{ magic, kind string }{
		{"\x28\xb5\x2f\xfd", "zstd-compressed"},
		{"\xfd7zXZ\x00", "xz-compressed"},
		{"\x1f\x8b", "gzip-compressed"},
		{"BZh", "bzip2-compressed"},
		{"KDMV", "VMDK"},
		{"# Disk DescriptorFile", "VMDK"},
		{"vhdxfile", "VHDX"},
		{"conectix", "VHD"},
		{"QED\x00", "QED"},
	}

	for _, prefix := range prefixes {
		if bytes.HasPrefix(header, []byte(prefix.magic)) {
			return prefix.kind
		}
	}

	if len(header) >= 68 && binary.LittleEndian.Uint32(header[64:68]) == 0xbeda107f {
		return "VDI"
	}

	return ""
}

func inspectQCOW2(header []byte) (string, error) {
	if len(header) < 72 {
		return "", invalid("truncated qcow2 header")
	}

	version := binary.BigEndian.Uint32(header[4:8])
	if version != 2 && version != 3 {
		return "", invalid("unsupported qcow2 version %d", version)
	}

	if binary.BigEndian.Uint64(header[8:16]) != 0 || binary.BigEndian.Uint32(header[16:20]) != 0 {
		return "", invalid("external qcow2 backing files are unsupported")
	}

	if binary.BigEndian.Uint32(header[32:36]) != 0 {
		return "", invalid("encrypted qcow2 images are unsupported")
	}

	// QEMU supports compression-type (3) and extended L2 (4). Reject external
	// data (2), dirty/corrupt (0/1), and every unknown incompatible feature.
	const supportedFeatures = uint64(1<<3 | 1<<4)

	if version == 3 && (len(header) < 104 || binary.BigEndian.Uint64(header[72:80]) & ^supportedFeatures != 0) {
		return "", invalid("unsupported qcow2 incompatible features (including external data files)")
	}

	return qcow2Format, nil
}

// fileArg names the i-th passed file in the child. Passing descriptors pins the
// opened inodes across pathname and pool-directory replacement.
func fileArg(i int) string {
	return fmt.Sprintf("/proc/self/fd/%d", 3+i)
}

// Info inspects a self-contained raw or qcow2 image using explicit input format.
func (c *Client) Info(ctx context.Context, file *os.File) (Info, error) {
	format, err := Inspect(file)
	if err != nil {
		return Info{}, err
	}

	return c.InfoFormat(ctx, file, format)
}

// InfoFormat inspects a destination with a known format without sniffing guest sectors.
func (c *Client) InfoFormat(ctx context.Context, file *os.File, format string) (Info, error) {
	if format != rawFormat && format != qcow2Format {
		return Info{}, invalid("unsupported image format %q", format)
	}

	stat, err := file.Stat()
	if err != nil {
		return Info{}, err
	}

	if !stat.Mode().IsRegular() {
		return Info{}, invalid("image is not a regular file")
	}

	data, err := c.run(ctx, []*os.File{file}, "info", "--output=json", "-f", format, fileArg(0))
	if err != nil {
		return Info{}, err
	}

	return decodeImageInfo(data, format)
}

func decodeImageInfo(data []byte, format string) (Info, error) {
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return Info{}, invalid("decode qemu-img info: %w", err)
	}

	if info.VirtualSize == 0 {
		return Info{}, invalid("image has no positive virtual size")
	}

	if info.Format != format || info.BackingFile != "" || info.FormatSpecific.Data.DataFile != "" {
		return Info{}, invalid("unsupported image format or external dependencies")
	}

	return info, nil
}

// Convert verifies capacity before writing, converts without backing dependencies,
// and grows the private result. A failed import never becomes a blank disk.
func (c *Client) Convert(ctx context.Context, spec ConvertSpec) error {
	capacity, err := validateCapacity(spec.Format, spec.Capacity)
	if err != nil {
		return err
	}

	spec.Capacity = capacity

	info, err := c.Info(ctx, spec.Source)
	if err != nil {
		return err
	}

	if spec.Capacity < info.VirtualSize {
		return invalid("requested capacity %d is below source virtual size %d", spec.Capacity, info.VirtualSize)
	}

	if err := independentDestination(spec); err != nil {
		return err
	}

	args := []string{"convert", "-f", info.Format, "-O", spec.Format, fileArg(0), fileArg(1)}
	if spec.Progress != nil {
		args = append([]string{"convert", "-p"}, args[1:]...)
	}

	_, err = c.runProgress(ctx, spec.Progress, []*os.File{spec.Source, spec.Destination}, args...)
	if err != nil {
		return err
	}

	result, err := c.resize(ctx, spec.Destination, spec.Format, spec.Capacity)
	if err == nil && spec.Result != nil {
		*spec.Result = result
	}

	return err
}

func independentDestination(spec ConvertSpec) error {
	destination, err := spec.Destination.Stat()
	if err != nil {
		return err
	}

	source, err := spec.Source.Stat()
	if err != nil {
		return err
	}

	if !destination.Mode().IsRegular() || os.SameFile(source, destination) {
		return invalid("conversion requires a separate regular destination file")
	}

	return nil
}

// Resize grows an offline private image. Published volumes use libvirt's fenced resize policy.
func (c *Client) Resize(ctx context.Context, file *os.File, format string, capacity uint64) error {
	_, err := c.resize(ctx, file, format, capacity)

	return err
}

func (c *Client) resize(ctx context.Context, file *os.File, format string, capacity uint64) (Info, error) {
	capacity, err := validateCapacity(format, capacity)
	if err != nil {
		return Info{}, err
	}

	info, err := c.InfoFormat(ctx, file, format)
	if err != nil {
		return Info{}, err
	}

	if capacity < info.VirtualSize {
		return Info{}, invalid("attempted shrink")
	}

	if capacity == info.VirtualSize {
		return info, nil
	}

	_, err = c.run(ctx, []*os.File{file}, "resize", "-f", format, fileArg(0), fmt.Sprint(capacity))
	if err != nil {
		return Info{}, err
	}

	info.VirtualSize = capacity

	return info, nil
}

const outputLimit = 64 << 10

type boundedOutput struct {
	data     []byte
	overflow bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	n := len(data)
	if len(data) > outputLimit-len(b.data) {
		data = data[:outputLimit-len(b.data)]
		b.overflow = true
	}

	b.data = append(b.data, data...)

	return n, nil
}

var conversionPercent = regexp.MustCompile(`\(([0-9]+(?:\.[0-9]+)?)/100%\)`)

type progressOutput struct {
	boundedOutput
	callback func(float64)
	carry    []byte
}

func (output *progressOutput) Write(data []byte) (int, error) {
	n, err := output.boundedOutput.Write(data)
	if output.callback == nil {
		return n, err
	}

	output.carry = append(output.carry, data...)

	window := output.carry
	for _, match := range conversionPercent.FindAllSubmatch(window, -1) {
		percent, parseErr := strconv.ParseFloat(string(match[1]), 64)
		if parseErr == nil && percent >= 0 && percent <= 100 {
			output.callback(percent)
		}
	}

	if len(window) > 24 {
		window = window[len(window)-24:]
	}

	output.carry = append(output.carry[:0], window...)

	return n, err
}

func (c *Client) run(ctx context.Context, files []*os.File, args ...string) ([]byte, error) {
	return c.runProgress(ctx, nil, files, args...)
}

func (c *Client) runProgress(ctx context.Context, progress func(float64), files []*os.File, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stdout := progressOutput{callback: progress}

	var stderr boundedOutput

	code, err := c.runner(ctx, c.binary, args, files, &stdout, &stderr)
	runtime.KeepAlive(files)

	if ctx.Err() != nil {
		return nil, errors.Join(ctx.Err(), err)
	}

	if err != nil {
		return nil, err
	}

	if code != 0 {
		return nil, &ExecError{ExitCode: code, Stderr: stderr.data}
	}

	if stdout.overflow && progress == nil {
		return nil, errors.New("qemu-img metadata exceeds output limit")
	}

	return stdout.data, nil
}

// validateCapacity returns the capacity qemu-img will actually provide.
// qcow2 sizes are rounded up to whole sectors, as libvirt does for blank volumes.
func validateCapacity(format string, capacity uint64) (uint64, error) {
	if format != rawFormat && format != qcow2Format {
		return 0, invalid("unsupported destination format %q", format)
	}

	if capacity == 0 {
		return 0, invalid("requested capacity must be positive")
	}

	if format == qcow2Format && capacity%512 != 0 {
		if capacity > math.MaxUint64-511 {
			return 0, invalid("qcow2 capacity %d is too large", capacity)
		}

		capacity = (capacity + 511) &^ 511
	}

	return capacity, nil
}

// Available cheaply checks the executable before any full source read.
func (c *Client) Available() error {
	_, err := exec.LookPath(c.binary)
	if err != nil {
		return toolStartError(err)
	}

	return nil
}

func toolStartError(err error) error {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ENOEXEC) || errors.Is(err, exec.ErrNotFound) {
		return &ToolUnavailableError{Err: err}
	}

	return err
}

// Preflight validates source header, minimum capacity and tool availability
// without hashing, staging or invoking an untrusted parser.
func (c *Client) Preflight(ctx context.Context, source *os.File, format string, capacity uint64) error {
	capacity, err := validateCapacity(format, capacity)
	if err != nil {
		return err
	}

	if err = c.Available(); err != nil {
		return err
	}

	inputFormat, err := Inspect(source)
	if err != nil {
		return err
	}

	var size uint64

	if inputFormat == qcow2Format {
		var header [32]byte
		if _, err := source.ReadAt(header[:], 0); err != nil {
			return err
		}

		size = binary.BigEndian.Uint64(header[24:32])
	} else {
		stat, err := source.Stat()
		if err != nil {
			return err
		}

		size = uint64(stat.Size())
	}

	if size == 0 || capacity < size {
		return invalid("requested capacity %d is below source virtual size %d (source must be nonempty)", capacity, size)
	}
	// Check executable/interpreter availability before a potentially enormous hash read.
	_, err = c.run(ctx, nil, "--version")

	return err
}
