// Package graphdb provides Apache AGE graph database operations.
//
// Executes openCypher queries for relationship traversal and pattern matching
// on PostgreSQL with the AGE extension.
//
// Example usage:
//
//	client, err := graphdb.NewClient(dbConn, "crosscodex_graph")
//	if err != nil {
//	    return err
//	}
//
//	result, err := client.Execute(ctx, `
//	    MATCH (c:Control)-[:MAPS_TO]->(req:Requirement)
//	    WHERE c.framework = $framework
//	    RETURN c, req
//	`, map[string]any{"framework": "NIST-800-53"})
package graphdb
