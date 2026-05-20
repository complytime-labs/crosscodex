package graphdb

import "time"

type Node struct {
	ID             string
	Label          string
	Properties     map[string]any
	ValidFrom      time.Time
	ValidTo        *time.Time
	CreatedBy      string
	CreationMethod string
}

type Edge struct {
	ID                string
	Label             string
	Source            string
	Target            string
	Properties        map[string]any
	ValidFrom         time.Time
	ValidTo           *time.Time
	DeterminedBy      string
	DeterminationType string
	Confidence        float64
	Supersedes        string
}

type Relationship struct {
	Source Node
	Edge   Edge
	Target Node
}

type RelationshipQuery struct {
	SourceLabel string
	TargetLabel string
	EdgeLabel   string
	Properties  map[string]any
}

type TraversalQuery struct {
	StartNode  string
	Direction  string // "outbound", "inbound", or "both"
	EdgeLabels []string
	MaxDepth   int // 0 = unlimited
}

type Path struct {
	Nodes []Node
	Edges []Edge
}
