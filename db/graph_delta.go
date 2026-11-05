package db

// GraphDelta replaces changed summaries and removes records from the previous
// window; it never appends windows or grows the client graph beyond the limit.
type GraphDelta struct {
	Nodes        []GraphViewNode `json:"nodes"`
	Edges        []GraphViewEdge `json:"edges"`
	RemovedNodes []string        `json:"removed_nodes"`
	RemovedEdges []GraphViewEdge `json:"removed_edges"`
	NodeOrder    []string        `json:"node_order"`
	EdgeOrder    []GraphViewEdge `json:"edge_order"`
}

func MakeGraphDelta(old, next GraphViewPage) GraphDelta {
	d := GraphDelta{Nodes: []GraphViewNode{}, Edges: []GraphViewEdge{}, RemovedNodes: []string{}, RemovedEdges: []GraphViewEdge{}, NodeOrder: []string{}, EdgeOrder: append([]GraphViewEdge{}, next.Edges...)}
	nodes := map[string]GraphViewNode{}
	for _, n := range old.Nodes {
		nodes[n.ID] = n
	}
	for _, n := range next.Nodes {
		p, ok := nodes[n.ID]
		if !ok || p != n {
			d.Nodes = append(d.Nodes, n)
		}
		delete(nodes, n.ID)
		d.NodeOrder = append(d.NodeOrder, n.ID)
	}
	for _, n := range old.Nodes {
		if _, ok := nodes[n.ID]; ok {
			d.RemovedNodes = append(d.RemovedNodes, n.ID)
		}
	}
	edges := map[GraphViewEdge]bool{}
	for _, e := range old.Edges {
		edges[e] = true
	}
	for _, e := range next.Edges {
		if !edges[e] {
			d.Edges = append(d.Edges, e)
		}
		delete(edges, e)
	}
	for _, e := range old.Edges {
		if edges[e] {
			d.RemovedEdges = append(d.RemovedEdges, e)
		}
	}
	return d
}
