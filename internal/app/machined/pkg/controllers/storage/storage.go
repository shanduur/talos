// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package storage provides the controllers related to LVM and storage virtualization.
package storage

import "time"

// pollInterval is how often the LVM status controllers re-scan the system in
// the absence of input events. LVM has no inotify-style change feed, so a
// poll is the only way to catch attribute changes (e.g. grow operations).
const pollInterval = 30 * time.Second
