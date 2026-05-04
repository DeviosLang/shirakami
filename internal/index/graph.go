package index

// InMemoryGraph holds the symbol graph in memory for microsecond-level BFS traversal.
// Loaded from PostgreSQL at startup, updated incrementally on reindex.
type InMemoryGraph struct {
	nodes    map[string]*SymbolNode
	inEdges  map[string][]SymbolEdge // target_id → []edges (find callers)
	outEdges map[string][]SymbolEdge // source_id → []edges (find callees)
}

// AffectedSymbol is one symbol found during impact analysis.
type AffectedSymbol struct {
	ID         string
	Name       string
	FilePath   string
	Repo       string
	Depth      int
	Confidence float64
	EdgeType   string // how this symbol is related (CALLS/IMPORTS/EXTENDS)
}

// NewInMemoryGraph creates an empty graph.
func NewInMemoryGraph() *InMemoryGraph {
	return &InMemoryGraph{
		nodes:    make(map[string]*SymbolNode),
		inEdges:  make(map[string][]SymbolEdge),
		outEdges: make(map[string][]SymbolEdge),
	}
}

// Load populates the graph from stored nodes and edges.
func (g *InMemoryGraph) Load(nodes []SymbolNode, edges []SymbolEdge) {
	for i := range nodes {
		g.nodes[nodes[i].ID] = &nodes[i]
	}
	for _, e := range edges {
		g.inEdges[e.TargetID] = append(g.inEdges[e.TargetID], e)
		g.outEdges[e.SourceID] = append(g.outEdges[e.SourceID], e)
	}
}

// NodeCount returns the number of nodes in the graph.
func (g *InMemoryGraph) NodeCount() int { return len(g.nodes) }

// EdgeCount returns the total number of edges in the graph.
func (g *InMemoryGraph) EdgeCount() int {
	count := 0
	for _, edges := range g.outEdges {
		count += len(edges)
	}
	return count
}

// GetNode returns a node by ID, or nil if not found.
func (g *InMemoryGraph) GetNode(id string) *SymbolNode {
	return g.nodes[id]
}

// FindNodesByName returns all nodes matching the given name (may be multiple due to overloads).
func (g *InMemoryGraph) FindNodesByName(name string) []*SymbolNode {
	var results []*SymbolNode
	for _, n := range g.nodes {
		if n.Name == name {
			results = append(results, n)
		}
	}
	return results
}

// FindNodesByFile returns all nodes in a given file.
func (g *InMemoryGraph) FindNodesByFile(repo, filePath string) []*SymbolNode {
	var results []*SymbolNode
	for _, n := range g.nodes {
		if n.Repo == repo && n.FilePath == filePath {
			results = append(results, n)
		}
	}
	return results
}

// Impact performs BFS traversal from startIDs in the specified direction.
//
// direction: "upstream" (find callers) or "downstream" (find callees)
// maxDepth: maximum traversal depth (1=direct, 2=indirect, 3=transitive)
// minConfidence: minimum edge confidence to follow (0.0 = follow all)
//
// Returns affected symbols grouped by depth, deduplicated.
func (g *InMemoryGraph) Impact(startIDs []string, direction string, maxDepth int, minConfidence float64) []AffectedSymbol {
	if maxDepth <= 0 {
		maxDepth = 3
	}

	visited := make(map[string]bool)
	var result []AffectedSymbol

	type bfsItem struct {
		id    string
		depth int
	}

	queue := make([]bfsItem, 0, len(startIDs))
	for _, id := range startIDs {
		queue = append(queue, bfsItem{id: id, depth: 0})
		visited[id] = true
	}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]

		if item.depth >= maxDepth {
			continue
		}

		// Get edges based on direction
		var edges []SymbolEdge
		if direction == "upstream" {
			edges = g.inEdges[item.id] // who calls me?
		} else {
			edges = g.outEdges[item.id] // who do I call?
		}

		for _, e := range edges {
			// Filter by confidence
			if e.Confidence < minConfidence {
				continue
			}

			// Determine the neighbor ID
			neighborID := e.SourceID
			if direction == "downstream" {
				neighborID = e.TargetID
			}

			if visited[neighborID] {
				continue
			}
			visited[neighborID] = true

			// Build affected symbol
			affected := AffectedSymbol{
				ID:         neighborID,
				Depth:      item.depth + 1,
				Confidence: e.Confidence,
				EdgeType:   e.Type,
			}

			// Enrich with node metadata if available
			if node := g.nodes[neighborID]; node != nil {
				affected.Name = node.Name
				affected.FilePath = node.FilePath
				affected.Repo = node.Repo
			}

			result = append(result, affected)
			queue = append(queue, bfsItem{id: neighborID, depth: item.depth + 1})
		}
	}

	return result
}
