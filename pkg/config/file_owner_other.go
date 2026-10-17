//go:build !unix

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package config: legacy "grove" -> "project" migration.
//
// This file holds the legacy grove->project migration support: the Reporter
// contract that the CLI, hub, and broker each render in their own idiom, and
// the warning for legacy environment variables that are no longer read. It
// is meant to be the only non-test file that still knows legacy "grove"
// names once the rest of the rename lands: the legacy names become
// unexported constants here, and every reader/writer in the rest of the tree
// goes through the canonical "project" names only.
//
// On-disk layout migration is expected to reuse the same Reporter and hook
// points (see ptone/scion#1919).

package config

import "os"

// fileOwnerUID reports no owner: this platform has no numeric file owner.
func fileOwnerUID(os.FileInfo) (int, bool) { return 0, false }
