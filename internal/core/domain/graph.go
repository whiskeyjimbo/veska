package domain

import (
	"errors"
	"fmt"
	"sort"
)

// Graph is a domain read projection — an in-memory bundle of Nodes and Edges
// scoped to a single (repo_id, branch) pair.  It is NOT a write aggregate;
// writes flow row-shaped through GraphRepository (a port).  Graph exists for
// read-time traversal only.
type Graph struct {
	RepoID string
	Branch string

	// nodes is keyed by NodeID.
	nodes map[NodeID]*Node

	// outgoing maps a NodeID to the edges that originate from it.
	outgoing map[NodeID][]*Edge

	// incoming maps a NodeID to the edges that terminate at it.
	incoming map[NodeID][]*Edge
}

// NewGraph constructs an empty Graph scoped to (repoID, branch).
// Both arguments must be non-empty.
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

// AddNode inserts a Node into the projection.  Returns an error if a node with
// the same ID already exists.
func (g *Graph) AddNode(n *Node) error {
	if _, exists := g.nodes[n.ID]; exists {
		return fmt.Errorf("graph: node %q already exists in graph (%s@%s)", n.ID, g.RepoID, g.Branch)
	}
	g.nodes[n.ID] = n
	return nil
}

// AddEdge inserts a directed Edge into the projection.  Both endpoint nodes
// must already be present; otherwise an error is returned.
func (g *Graph) AddEdge(e *Edge) error {
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

// Nodes returns every Node in the projection, ordered by ascending NodeID.
// The deterministic order lets read-time consumers (e.g. the wiki
// entry_points ranking) enumerate candidates without map-iteration order
// leaking into their output.
func (g *Graph) Nodes() []*Node {
	out := make([]*Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Node returns the Node for the given ID, or (nil, false) if not present.
func (g *Graph) Node(id NodeID) (*Node, bool) {
	n, ok := g.nodes[id]
	return n, ok
}

// OutgoingEdges returns all edges whose source is id.  Returns an empty
// (non-nil) slice if none exist.
func (g *Graph) OutgoingEdges(id NodeID) []*Edge {
	if edges, ok := g.outgoing[id]; ok {
		return edges
	}
	return []*Edge{}
}

// IncomingEdges returns all edges whose target is id.  Returns an empty
// (non-nil) slice if none exist.
func (g *Graph) IncomingEdges(id NodeID) []*Edge {
	if edges, ok := g.incoming[id]; ok {
		return edges
	}
	return []*Edge{}
}
