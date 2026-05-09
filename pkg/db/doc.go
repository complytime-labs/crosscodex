// Package db provides PostgreSQL connection pooling and transaction management.
//
// Handles connection lifecycle, prepared statements, and ACID transactions.
//
// Example usage:
//
//	conn, err := db.Connect(ctx, "postgres://user:pass@localhost/dbname")
//	if err != nil {
//	    return err
//	}
//	defer conn.Close()
//
//	tx, err := conn.Begin(ctx)
//	if err != nil {
//	    return err
//	}
//	defer tx.Rollback()
//
//	_, err = tx.Exec(ctx, "INSERT INTO users (name) VALUES ($1)", "Alice")
//	if err != nil {
//	    return err
//	}
//
//	err = tx.Commit()
package db
