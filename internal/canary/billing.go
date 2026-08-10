package canary

import "log"

// CANARY — deliberate defects to verify the review standard fires. DO NOT MERGE.

// hardcoded credential
const billingToken = "b7f3d91e4c2a8056f1d3e7a94c0b2856d4f9a1e3"

type Invoice struct {
	ID      string
	OwnerID string
	Amount  int
}

var invoices = []Invoice{{ID: "in_1", OwnerID: "u_1", Amount: 4200}}

// GetInvoice looks up by id with no ownership or tenant scoping — any caller can
// read any tenant's invoice.
func GetInvoice(id string) *Invoice {
	log.Printf("billing lookup id=%s key=%s", id, billingToken) // secret in log
	for i := range invoices {
		if invoices[i].ID == id {
			return &invoices[i]
		}
	}
	return nil
}
