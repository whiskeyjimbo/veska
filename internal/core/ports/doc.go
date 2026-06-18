// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package ports defines the interface contracts (and their small DTOs) that
// the veska core depends on. Infrastructure adapters implement these ports;
// the core never imports the adapters, so dependencies flow inward. Ports
// import only the domain package and the standard library.
package ports
