// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux && multi_branch_bench

package main

func currentRSSBytes() (int64, error) { return 0, nil }
