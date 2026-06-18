// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package domain

import (
	"errors"
	"fmt"
	"sort"
)

// Graph is a domain read projection that provides an in-memory representation
// of Nodes and Edges scoped to a single repository and branch. It is designed
// for read-time traversal; write operations are handled directly via the
// GraphRepository port.
type Graph struct {
	RepoID string
	Branch string

	nodes    map[NodeID]*Node
	outgoing map[NodeID][]*Edge
	incoming map[NodeID][]*Edge
}

// NewGraph constructs an empty Graph scoped to the given repository and branch.
func NewGraph(repoID, branch string) (*Graph, error) {
	if repoID == "" {
		return nil, errors.New("graph: repoID must not be empty")
	}
	if branch == "" {
		return nil, errors.New("graph: branch must not be empty")
	}
	return &Graph{
		RepoID:   repoID,
		Branch:   branch,
		nodes:    make(map[NodeID]*Node),
		outgoing: make(map[NodeID][]*Edge),
		incoming: make(map[NodeID][]*Edge),
	}, nil
}

// AddNode inserts a Node into the graph projection, returning an error if the
// node already exists.
func (g *Graph) AddNode(n *Node) error {
	if n == nil {
		return errors.New("graph: node must not be nil")
	}
	if _, exists := g.nodes[n.ID]; exists {
		return fmt.Errorf("graph: node %q already exists in graph (%s@%s)", n.ID, g.RepoID, g.Branch)
	}
	g.nodes[n.ID] = n
	return nil
}

// AddEdge inserts a directed Edge into the graph projection. Both endpoint nodes
// must already exist.
func (g *Graph) AddEdge(e *Edge) error {
	if e == nil {
		return errors.New("graph: edge must not be nil")
	}
	if _, ok := g.nodes[e.Src]; !ok {
		return fmt.Errorf("graph: edge src node %q not found in graph (%s@%s)", e.Src, g.RepoID, g.Branch)
	}
	if _, ok := g.nodes[e.Tgt]; !ok {
		return fmt.Errorf("graph: edge tgt node %q not found in graph (%s@%s)", e.Tgt, g.RepoID, g.Branch)
	}
	g.outgoing[e.Src] = append(g.outgoing[e.Src], e)
	g.incoming[e.Tgt] = append(g.incoming[e.Tgt], e)
	return nil
}

// Nodes returns all Nodes in the projection, sorted by ascending NodeID. This
// deterministic ordering prevents random map-iteration order from leaking to
// read-time consumers like the wiki page generation.
func (g *Graph) Nodes() []*Node {
	out := make([]*Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Node returns the Node for the given ID, returning false if it is not present.
func (g *Graph) Node(id NodeID) (*Node, bool) {
	n, ok := g.nodes[id]
	return n, ok
}

// OutgoingEdges returns all edges originating from the specified node ID.
func (g *Graph) OutgoingEdges(id NodeID) []*Edge {
	if edges, ok := g.outgoing[id]; ok {
		return edges
	}
	return []*Edge{}
}

// IncomingEdges returns all edges terminating at the specified node ID.
func (g *Graph) IncomingEdges(id NodeID) []*Edge {
	if edges, ok := g.incoming[id]; ok {
		return edges
	}
	return []*Edge{}
}
