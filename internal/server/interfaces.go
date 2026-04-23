package server

import (
	"context"

	"github.com/olusolaa/paybook/internal/payments"
	"github.com/olusolaa/paybook/internal/reconciliation"
)

// PaymentsService is the handler's view of the payment application pipeline.
// Defined at the consumer (this package) so tests can substitute a fake
// without importing the full payments implementation or standing up a
// database. The concrete *payments.Service satisfies it.
type PaymentsService interface {
	ValidateAndParse(payments.RawInput) (payments.Payment, error)
	Apply(context.Context, payments.Payment) (*payments.Outcome, error)
}

// ReconciliationService is the balance-read surface used by the balance
// handler. Same pattern as PaymentsService: consumer-defined, concrete
// *reconciliation.Service satisfies it.
type ReconciliationService interface {
	ForCustomer(context.Context, string) (*reconciliation.CustomerBalance, error)
}
