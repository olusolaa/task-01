package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/olusolaa/paybook/internal/money"
)

// Clock is injectable so tests can drive transaction-date skew checks
// deterministically. The production wiring uses realClock.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// Service applies validated payment notifications against the ledger.
type Service struct {
	repo           *Repo
	clock          Clock
	clockSkewGrace time.Duration
}

func NewService(repo *Repo, clockSkewGrace time.Duration) *Service {
	return &Service{
		repo:           repo,
		clock:          realClock{},
		clockSkewGrace: clockSkewGrace,
	}
}

// WithClock replaces the default clock; used by tests.
func (s *Service) WithClock(c Clock) *Service {
	s.clock = c
	return s
}

// RawInput is the raw webhook payload after JSON decoding. It is parsed
// into a Payment by ValidateAndParse so the rest of the service works
// in domain types, not strings.
type RawInput struct {
	CustomerID           string
	PaymentStatus        string
	TransactionAmount    string
	TransactionDate      string
	TransactionReference string
}

// The bank sends transaction_date as a naive "YYYY-MM-DD HH:MM:SS".
// We parse as UTC by policy; the assumption is stated in the README.
const transactionDateLayout = "2006-01-02 15:04:05"

// ValidateAndParse turns the raw payload into a Payment and returns any
// validation error wrapped with the relevant sentinel so the handler can
// map to an HTTP status.
func (s *Service) ValidateAndParse(in RawInput) (Payment, error) {
	if in.CustomerID == "" || in.TransactionReference == "" {
		return Payment{}, fmt.Errorf("%w: customer_id and transaction_reference are required", ErrInvalidPayload)
	}

	status, err := ParseStatus(in.PaymentStatus)
	if err != nil {
		return Payment{}, fmt.Errorf("%w: %q", ErrInvalidStatus, in.PaymentStatus)
	}

	amount, err := money.ParseNaira(in.TransactionAmount)
	if err != nil {
		return Payment{}, fmt.Errorf("%w: %s", ErrInvalidAmount, err)
	}

	txDate, err := time.Parse(transactionDateLayout, in.TransactionDate)
	if err != nil {
		return Payment{}, fmt.Errorf("%w: expected %q", ErrInvalidDate, transactionDateLayout)
	}
	txDate = txDate.UTC()

	now := s.clock.Now()
	if txDate.After(now.Add(s.clockSkewGrace)) {
		return Payment{}, fmt.Errorf("%w: transaction_date more than %s in the future", ErrInvalidDate, s.clockSkewGrace)
	}

	return Payment{
		CustomerID:           in.CustomerID,
		TransactionReference: in.TransactionReference,
		AmountKobo:           amount,
		Status:               status,
		TransactionDate:      txDate,
	}, nil
}

// Apply executes the full pipeline for a single payment: replay check,
// deployment lock, outcome decision, and atomic persistence. The returned
// Outcome holds the exact bytes to serve to the client.
func (s *Service) Apply(ctx context.Context, p Payment) (*Outcome, error) {
	var out *Outcome

	err := s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		if stored, err := s.repo.LookupByTxnRef(ctx, tx, p.TransactionReference); err != nil {
			return err
		} else if stored != nil {
			out = &Outcome{
				Replayed: true,
				Result:   stored.Result,
				Status:   stored.Status,
				Body:     stored.Body,
			}
			return nil
		}

		dep, err := s.repo.LockRoutingDeployment(ctx, tx, p.CustomerID)
		if err != nil {
			return err
		}

		d := decide(p, dep)
		rec := RecordIn{
			Payment:            p,
			DeploymentID:       &dep.ID,
			Result:             d.result,
			RejectReason:       d.rejectReason,
			ResponseStatus:     d.status,
			ResponseBody:       d.body,
			AppliedBalanceKobo: d.newBalance,
			NewDeploymentState: d.newDeploymentState,
		}
		inserted, err := s.repo.RecordPayment(ctx, tx, rec)
		if err != nil {
			return err
		}

		if !inserted {
			stored, err := s.repo.LookupByTxnRef(ctx, tx, p.TransactionReference)
			if err != nil {
				return err
			}
			if stored == nil {
				return errors.New("conflict without stored response; invariant violated")
			}
			out = &Outcome{
				Replayed: true,
				Result:   stored.Result,
				Status:   stored.Status,
				Body:     stored.Body,
			}
			return nil
		}

		out = &Outcome{
			Replayed: false,
			Result:   d.result,
			Status:   d.status,
			Body:     d.body,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// decision is the full output of the policy function. It is private to
// the service layer.
type decision struct {
	result             Result
	rejectReason       string
	status             int
	body               []byte
	newDeploymentState DeploymentState
	newBalance         *money.Kobo
}

// decide is the pure policy: given a validated payment and the current
// state of the locked deployment, what outcome do we emit?
//
// Order matters. Non-COMPLETE statuses are recorded but do not touch
// balance. An inactive deployment rejects before any amount check.
// Overpayment rejects before any balance change.
func decide(p Payment, dep *Deployment) decision {
	switch {
	case p.Status != StatusComplete:
		body := mustJSON(recordedBody{
			Status:               "recorded",
			TransactionReference: p.TransactionReference,
			PaymentStatus:        string(p.Status),
			Detail:               "payment recorded for audit; status is not COMPLETE",
		})
		return decision{
			result: ResultRecorded,
			status: http.StatusAccepted,
			body:   body,
		}

	case dep.State != DeploymentActive:
		body := mustJSON(rejectedBody{
			Error:                "deployment_inactive",
			TransactionReference: p.TransactionReference,
			DeploymentState:      strPtr(string(dep.State)),
		})
		return decision{
			result:       ResultRejected,
			rejectReason: "deployment_inactive",
			status:       http.StatusConflict,
			body:         body,
		}

	case p.AmountKobo > dep.CurrentBalanceKobo:
		outstanding := dep.CurrentBalanceKobo.Int64()
		body := mustJSON(rejectedBody{
			Error:                "overpayment",
			TransactionReference: p.TransactionReference,
			OutstandingKobo:      &outstanding,
		})
		return decision{
			result:       ResultRejected,
			rejectReason: "overpayment",
			status:       http.StatusConflict,
			body:         body,
		}
	}

	newBalance := dep.CurrentBalanceKobo - p.AmountKobo
	newState := DeploymentActive
	if newBalance == 0 {
		newState = DeploymentFullyRepaid
	}

	body := mustJSON(appliedBody{
		Status:               "applied",
		TransactionReference: p.TransactionReference,
		DeploymentID:         dep.ID,
		AmountAppliedKobo:    p.AmountKobo.Int64(),
		BalanceAfterKobo:     newBalance.Int64(),
		DeploymentState:      string(newState),
	})
	return decision{
		result:             ResultApplied,
		status:             http.StatusCreated,
		body:               body,
		newDeploymentState: newState,
		newBalance:         &newBalance,
	}
}

type appliedBody struct {
	Status               string `json:"status"`
	TransactionReference string `json:"transaction_reference"`
	DeploymentID         string `json:"deployment_id"`
	AmountAppliedKobo    int64  `json:"amount_applied_kobo"`
	BalanceAfterKobo     int64  `json:"balance_after_kobo"`
	DeploymentState      string `json:"deployment_state"`
}

type recordedBody struct {
	Status               string `json:"status"`
	TransactionReference string `json:"transaction_reference"`
	PaymentStatus        string `json:"payment_status"`
	Detail               string `json:"detail"`
}

type rejectedBody struct {
	Error                string  `json:"error"`
	TransactionReference string  `json:"transaction_reference"`
	OutstandingKobo      *int64  `json:"outstanding_kobo,omitempty"`
	DeploymentState      *string `json:"deployment_state,omitempty"`
}

func strPtr(s string) *string { return &s }

// mustJSON marshals a struct that cannot fail to marshal. Used only with
// local types we fully control; a marshal error here is a programmer bug.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Errorf("marshal response body: %w", err))
	}
	return b
}
