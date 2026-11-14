// Package webhook sends the events of Lajstrom's writes to webhooks (docs/spec/webhooks.md):
// Outbox writes an event's deliveries in the transaction of its audit row, Worker delivers them
// from any replica under a claim, Deliverer signs each request and sends it through the egress
// guard, so that a webhook reaches only public addresses over https unless the operator allowed
// its host.
package webhook
