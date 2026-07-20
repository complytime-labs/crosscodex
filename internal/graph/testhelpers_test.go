package graph_test

import (
	"context"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

// mockGraphDB is a test double for graphdb.GraphDB.
type mockGraphDB struct {
	createGraphFunc        func(ctx context.Context, tenant string) error
	createNodeFunc         func(ctx context.Context, tenant string, node graphdb.Node) error
	createEdgeFunc         func(ctx context.Context, tenant, sourceID, targetID string, edge graphdb.Edge) error
	createRequiresEdgeFunc func(ctx context.Context, tenant string, reqEdge graphdb.RequiresEdge) error
	queryRelationshipsFunc func(ctx context.Context, tenant string, query graphdb.RelationshipQuery) ([]graphdb.Relationship, error)
	traverseFunc           func(ctx context.Context, tenant string, query graphdb.TraversalQuery) ([]graphdb.Path, error)
	queryAsOfFunc          func(ctx context.Context, tenant string, query graphdb.RelationshipQuery, asOf time.Time) ([]graphdb.Relationship, error)
	getNodeFunc            func(ctx context.Context, tenant, nodeID string) (*graphdb.Node, error)
	getEdgeFunc            func(ctx context.Context, tenant, edgeID string) (*graphdb.EdgeWithEndpoints, error)
	bulkCreateEdgesFunc    func(ctx context.Context, tenant string, edges []graphdb.BulkEdge) ([]string, error)
	executeQueryFunc       func(ctx context.Context, tenant, cypher string, params map[string]string) ([]graphdb.QueryRow, error)
	supersedeFactFunc      func(ctx context.Context, tenant string, req graphdb.SupersedeRequest) (bool, error)
}

func (m *mockGraphDB) CreateGraph(ctx context.Context, tenant string) error {
	if m.createGraphFunc != nil {
		return m.createGraphFunc(ctx, tenant)
	}
	return nil
}

func (m *mockGraphDB) CreateNode(ctx context.Context, tenant string, node graphdb.Node) error {
	if m.createNodeFunc != nil {
		return m.createNodeFunc(ctx, tenant, node)
	}
	return nil
}

func (m *mockGraphDB) CreateEdge(ctx context.Context, tenant, sourceID, targetID string, edge graphdb.Edge) error {
	if m.createEdgeFunc != nil {
		return m.createEdgeFunc(ctx, tenant, sourceID, targetID, edge)
	}
	return nil
}

func (m *mockGraphDB) CreateRequiresEdge(ctx context.Context, tenant string, reqEdge graphdb.RequiresEdge) error {
	if m.createRequiresEdgeFunc != nil {
		return m.createRequiresEdgeFunc(ctx, tenant, reqEdge)
	}
	return nil
}

func (m *mockGraphDB) QueryRelationships(ctx context.Context, tenant string, query graphdb.RelationshipQuery) ([]graphdb.Relationship, error) {
	if m.queryRelationshipsFunc != nil {
		return m.queryRelationshipsFunc(ctx, tenant, query)
	}
	return nil, nil
}

func (m *mockGraphDB) Traverse(ctx context.Context, tenant string, query graphdb.TraversalQuery) ([]graphdb.Path, error) {
	if m.traverseFunc != nil {
		return m.traverseFunc(ctx, tenant, query)
	}
	return nil, nil
}

func (m *mockGraphDB) QueryAsOf(ctx context.Context, tenant string, query graphdb.RelationshipQuery, asOf time.Time) ([]graphdb.Relationship, error) {
	if m.queryAsOfFunc != nil {
		return m.queryAsOfFunc(ctx, tenant, query, asOf)
	}
	return nil, nil
}

func (m *mockGraphDB) GetNode(ctx context.Context, tenant, nodeID string) (*graphdb.Node, error) {
	if m.getNodeFunc != nil {
		return m.getNodeFunc(ctx, tenant, nodeID)
	}
	return nil, graphdb.ErrNodeNotFound
}

func (m *mockGraphDB) GetEdge(ctx context.Context, tenant, edgeID string) (*graphdb.EdgeWithEndpoints, error) {
	if m.getEdgeFunc != nil {
		return m.getEdgeFunc(ctx, tenant, edgeID)
	}
	return nil, graphdb.ErrEdgeNotFound
}

func (m *mockGraphDB) BulkCreateEdges(ctx context.Context, tenant string, edges []graphdb.BulkEdge) ([]string, error) {
	if m.bulkCreateEdgesFunc != nil {
		return m.bulkCreateEdgesFunc(ctx, tenant, edges)
	}
	return nil, nil
}

func (m *mockGraphDB) ExecuteQuery(ctx context.Context, tenant, cypher string, params map[string]string) ([]graphdb.QueryRow, error) {
	if m.executeQueryFunc != nil {
		return m.executeQueryFunc(ctx, tenant, cypher, params)
	}
	return nil, nil
}

func (m *mockGraphDB) SupersedeFact(ctx context.Context, tenant string, req graphdb.SupersedeRequest) (bool, error) {
	if m.supersedeFactFunc != nil {
		return m.supersedeFactFunc(ctx, tenant, req)
	}
	return false, nil
}

// mockVectorDB is a test double for vectordb.VectorDB.
type mockVectorDB struct {
	storeEmbeddingFunc func(ctx context.Context, tenantID string, embedding vectordb.Embedding) error
	storeBatchFunc     func(ctx context.Context, tenantID string, embeddings []vectordb.Embedding) error
	findSimilarFunc    func(ctx context.Context, tenantID string, query vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error)
	deleteByModelFunc  func(ctx context.Context, tenantID, catalogID, model string) error
}

func (m *mockVectorDB) StoreEmbedding(ctx context.Context, tenantID string, embedding vectordb.Embedding) error {
	if m.storeEmbeddingFunc != nil {
		return m.storeEmbeddingFunc(ctx, tenantID, embedding)
	}
	return nil
}

func (m *mockVectorDB) StoreBatch(ctx context.Context, tenantID string, embeddings []vectordb.Embedding) error {
	if m.storeBatchFunc != nil {
		return m.storeBatchFunc(ctx, tenantID, embeddings)
	}
	return nil
}

func (m *mockVectorDB) FindSimilar(ctx context.Context, tenantID string, query vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
	if m.findSimilarFunc != nil {
		return m.findSimilarFunc(ctx, tenantID, query)
	}
	return nil, nil
}

func (m *mockVectorDB) DeleteByModel(ctx context.Context, tenantID, catalogID, model string) error {
	if m.deleteByModelFunc != nil {
		return m.deleteByModelFunc(ctx, tenantID, catalogID, model)
	}
	return nil
}

// mockNATSClient is a test double for natsbus.Client.
type mockNATSClient struct {
	queueSubscribeFunc func(ctx context.Context, subject, queue string, handler natsbus.MessageHandler) (natsbus.Subscription, error)
}

func (m *mockNATSClient) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (m *mockNATSClient) PublishWithHeaders(_ context.Context, _ string, _ []byte, _ map[string][]string) error {
	return nil
}
func (m *mockNATSClient) Subscribe(_ context.Context, _ string, _ natsbus.MessageHandler) (natsbus.Subscription, error) {
	return nil, nil
}
func (m *mockNATSClient) QueueSubscribe(ctx context.Context, subject, queue string, handler natsbus.MessageHandler) (natsbus.Subscription, error) {
	if m.queueSubscribeFunc != nil {
		return m.queueSubscribeFunc(ctx, subject, queue, handler)
	}
	return &mockSubscription{}, nil
}
func (m *mockNATSClient) CreateStream(_ context.Context, _ natsbus.StreamConfig) error { return nil }
func (m *mockNATSClient) DeleteStream(_ context.Context, _ string) error               { return nil }
func (m *mockNATSClient) Close() error                                                 { return nil }

// mockSubscription is a test double for natsbus.Subscription.
type mockSubscription struct {
	drainFunc func() error
}

func (m *mockSubscription) Unsubscribe() error { return nil }
func (m *mockSubscription) Drain() error {
	if m.drainFunc != nil {
		return m.drainFunc()
	}
	return nil
}
