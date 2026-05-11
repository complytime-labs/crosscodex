// Package natsbus provides NATS JetStream messaging client.
//
// Handles publish/subscribe messaging with stream management and
// tenant isolation.
//
// Example usage:
//
//	client, err := natsbus.Connect(ctx, "nats://localhost:4222")
//	if err != nil {
//	    return err
//	}
//	defer client.Close()
//
//	err = client.Publish(ctx, "ingestion.documents", data)
//	if err != nil {
//	    return err
//	}
//
//	sub, err := client.Subscribe(ctx, "analysis.results", handler)
package natsbus
