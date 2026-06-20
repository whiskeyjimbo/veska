// SPDX-License-Identifier: AGPL-3.0-only

// Package domain contains the core domain types for the veska module:
// entities (Node, Edge, Task), the read-projection aggregate (Graph), and
// the value types and enums they compose. It depends only on the standard
// library; coupling flows inward through the ports package, never outward
// to infrastructure.
package domain
