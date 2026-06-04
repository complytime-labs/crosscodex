// Package graphdb provides Apache AGE graph database operations.
//
// Executes openCypher queries for relationship traversal and pattern matching
// on PostgreSQL with the AGE extension. Each tenant is isolated in its own
// named graph (crosscodex_{tenant_id}).
//
// The package accepts a *sql.DB from the shared connection pool and does not
// manage its own connections.
//
// Example usage:
//
//	client, err := graphdb.New(db)
//
//	err := client.CreateNode(ctx, "acme", graphdb.Node{
//	    ID:        "ctrl-1",
//	    Label:     "Requirement",
//	    ValidFrom: time.Now(),
//	    CreatedBy: "import-job-1",
//	})
package graphdb
