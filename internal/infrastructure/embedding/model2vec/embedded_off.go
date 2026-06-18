// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !embed_model

package model2vec

// Embedded returns a negative indicator since no model is compiled into this thin build.
func Embedded() (*Provider, bool) { return nil, false }
