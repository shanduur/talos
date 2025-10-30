// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package block

import (
	"encoding"
	"strings"

	"gopkg.in/yaml.v3"
)

// Check interfaces.
var (
	_ encoding.TextMarshaler   = Size{}
	_ encoding.TextUnmarshaler = (*PercentageSize)(nil)
	_ yaml.IsZeroer            = Size{}
)

// Size is either a PercentageSize or ByteSize.
type Size struct {
	ps *PercentageSize
	bs *ByteSize
}

// MustSize returns a new Size with the given value.
//
// It panics if the value is invalid.
func MustSize(value string) Size {
	var s Size

	if err := s.UnmarshalText([]byte(value)); err != nil {
		panic(err)
	}

	return s
}

// MustPercentageSize returns a new Size with the given PercentageSize value.
//
// It panics if the value is invalid.
func MustPercentageSize(value string) Size {
	var ps PercentageSize

	if err := ps.UnmarshalText([]byte(value)); err != nil {
		panic(err)
	}

	return Size{ps: &ps}
}

// MustByteSize returns a new Size with the given ByteSize value.
//
// It panics if the value is invalid.
func MustByteSize(value string) Size {
	var bs ByteSize

	if err := bs.UnmarshalText([]byte(value)); err != nil {
		panic(err)
	}

	return Size{bs: &bs}
}

// Value returns the value.
func (s Size) Value() uint64 {
	if s.bs != nil {
		return s.bs.Value()
	}

	if s.ps != nil {
		return s.ps.Value()
	}

	return 0
}

// RelativeValue returns the relative value.
func (s Size) RelativeValue(in uint64) uint64 {
	if s.bs != nil {
		return s.bs.RelativeValue(in)
	}

	if s.ps != nil {
		return s.ps.RelativeValue(in)
	}

	return 0
}

// MarshalText implements encoding.TextMarshaler.
func (s Size) MarshalText() ([]byte, error) {
	if s.bs != nil {
		return s.bs.MarshalText()
	}

	if s.ps != nil {
		return s.ps.MarshalText()
	}

	return nil, nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Size) UnmarshalText(text []byte) error {
	if string(text) == "" {
		return nil
	}

	if strings.Contains(string(text), "%") {
		var ps PercentageSize
		if err := ps.UnmarshalText(text); err != nil {
			return err
		}

		s.ps = &ps
	} else {
		var bs ByteSize
		if err := bs.UnmarshalText(text); err != nil {
			return err
		}

		s.bs = &bs
	}

	return nil
}

// IsZero implements yaml.IsZeroer.
func (s Size) IsZero() bool {
	return (s.ps == nil || s.ps.IsZero()) && (s.bs == nil || s.bs.IsZero())
}
