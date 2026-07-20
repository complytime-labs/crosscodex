// Package graphdb provides Apache AGE graph database operations.
//
// Executes openCypher queries for relationship traversal, entity retrieval,
// bulk edge creation, and temporal management on PostgreSQL with the AGE
// extension. Each tenant is isolated in its own named graph
// (crosscodex_{tenant_id}).
//
// The package accepts a *sql.DB from the shared connection pool and does not
// manage its own connections.
//
// Example — create and retrieve a node:
//
//	client, err := graphdb.New(db)
//
//	err := client.CreateNode(ctx, "acme", graphdb.Node{
//	    ID:        "ctrl-1",
//	    Label:     "Requirement",
//	    ValidFrom: time.Now(),
//	    CreatedBy: "import-job-1",
//	})
//
//	node, err := client.GetNode(ctx, "acme", "ctrl-1")
package graphdb
