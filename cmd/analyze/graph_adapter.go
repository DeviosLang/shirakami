package main

import (
	"github.com/DeviosLang/shirakami/internal/agent"
	"github.com/DeviosLang/shirakami/internal/index"
)

// graphAdapter bridges index.InMemoryGraph → agent.IndexGraph interface.
type graphAdapter struct {
	graph *index.InMemoryGraph
}

func (a *graphAdapter) Impact(startIDs []string, direction string, maxDepth int, minConfidence float64) []agent.IndexAffectedSymbol {
	affected := a.graph.Impact(startIDs, direction, maxDepth, minConfidence)
	result := make([]agent.IndexAffectedSymbol, len(affected))
	for i, s := range affected {
		result[i] = agent.IndexAffectedSymbol{
			ID:         s.ID,
			Name:       s.Name,
			FilePath:   s.FilePath,
			Repo:       s.Repo,
			Depth:      s.Depth,
			Confidence: s.Confidence,
			EdgeType:   s.EdgeType,
		}
	}
	return result
}

func (a *graphAdapter) FindNodesByName(name string) []agent.IndexSymbolNode {
	nodes := a.graph.FindNodesByName(name)
	result := make([]agent.IndexSymbolNode, len(nodes))
	for i, n := range nodes {
		result[i] = agent.IndexSymbolNode{
			ID:        n.ID,
			Repo:      n.Repo,
			FilePath:  n.FilePath,
			Name:      n.Name,
			Kind:      n.Kind,
			StartLine: n.StartLine,
			EndLine:   n.EndLine,
		}
	}
	return result
}

func (a *graphAdapter) NodeCount() int {
	return a.graph.NodeCount()
}
