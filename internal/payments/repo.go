package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/olusolaa/paybook/internal/money"
)

// Querier is the subset of pgx used by the repository. Both *pgxpool.Pool
// and pgx.Tx satisfy it, so repo methods work inside or outside a tx.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Repo is stateless SQL against a pgx pool. All multi-statement atomic work
// goes through WithTx so the tx boundary is explicit at the call site.
type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

// Pool exposes the underlying pool for reads that do not need a tx.
func (r *Repo) Pool() *pgxpool.Pool { return r.pool }

// WithTx runs fn inside a READ COMMITTED transaction. Rollback is deferred
// defensively; committing on success is explicit.
func (r *Repo) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// StoredResponse is the persisted replay record for an already-applied
// transaction reference.
type StoredResponse struct {
	Status int
	Body   []byte
	Result Result
}

// LookupByTxnRef returns the stored response for a transaction reference,
// or nil if the reference has never been seen.
func (r *Repo) LookupByTxnRef(ctx context.Context, q Querier, ref string) (*StoredResponse, error) {
	var out StoredResponse
	var status int16
	err := q.QueryRow(ctx, `
		SELECT response_status, response_body, result
		FROM payments
		WHERE transaction_reference = $1
	`, ref).Scan(&status, &out.Body, &out.Result)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup txn ref: %w", err)
	}
	out.Status = int(status)
	return &out, nil
}

// LockRoutingDeployment resolves the customer's routing deployment and
// locks its row for the surrounding transaction.
//
// Policy (stated in ADR 001): prefer the oldest active deployment. If no
// deployment is active, return the oldest inactive one so the caller can
// respond with a specific "deployment_inactive" error instead of a vague
// "not found".
//
// Errors: ErrCustomerNotFound if the customer id is unknown, or
// ErrDeploymentNotFound if the customer has no deployments at all.
func (r *Repo) LockRoutingDeployment(ctx context.Context, q Querier, customerID string) (*Deployment, error) {
	var exists bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM customers WHERE id = $1)`,
		customerID,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check customer: %w", err)
	}
	if !exists {
		return nil, ErrCustomerNotFound
	}

	var dep Deployment
	var value, balance int64
	var state string
	err := q.QueryRow(ctx, `
		SELECT id, customer_id, value_kobo, current_balance_kobo, state, started_at
		FROM deployments
		WHERE customer_id = $1
		ORDER BY (state = 'ACTIVE') DESC, started_at ASC
		LIMIT 1
		FOR UPDATE
	`, customerID).Scan(&dep.ID, &dep.CustomerID, &value, &balance, &state, &dep.StartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeploymentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock deployment: %w", err)
	}
	dep.ValueKobo = money.Kobo(value)
	dep.CurrentBalanceKobo = money.Kobo(balance)
	dep.State = DeploymentState(state)
	return &dep, nil
}

// RecordIn carries every field required to persist a payment decision
// atomically. The service fills it in; the repo is a dumb writer.
type RecordIn struct {
	Payment            Payment
	DeploymentID       *string
	Result             Result
	RejectReason       string // non-empty iff Result == ResultRejected
	ResponseStatus     int
	ResponseBody       []byte
	AppliedBalanceKobo *money.Kobo     // set iff Result == ResultApplied
	NewDeploymentState DeploymentState // "" to leave state unchanged
}

// RecordPayment tries to insert the payment row.
//
// Returns (true, nil) if the insert happened. If Result=Applied it also
// updates the deployment balance and optionally transitions state in the
// same transaction.
//
// Returns (false, nil) if transaction_reference already existed (another
// transaction beat us); caller should re-lookup the stored response.
func (r *Repo) RecordPayment(ctx context.Context, q Querier, in RecordIn) (bool, error) {
	if err := validateRecordIn(in); err != nil {
		return false, err
	}

	var depArg any
	if in.DeploymentID != nil {
		depArg = *in.DeploymentID
	}
	var rejectArg any
	if in.RejectReason != "" {
		rejectArg = in.RejectReason
	}
	var appliedArg any
	if in.AppliedBalanceKobo != nil {
		appliedArg = in.AppliedBalanceKobo.Int64()
	}

	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO payments (
			transaction_reference, customer_id, deployment_id,
			amount_kobo, status, result, reject_reason,
			response_status, response_body, transaction_date, applied_balance_kobo
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (transaction_reference) DO NOTHING
		RETURNING id
	`,
		in.Payment.TransactionReference,
		in.Payment.CustomerID,
		depArg,
		in.Payment.AmountKobo.Int64(),
		string(in.Payment.Status),
		string(in.Result),
		rejectArg,
		int16(in.ResponseStatus),
		in.ResponseBody,
		in.Payment.TransactionDate,
		appliedArg,
	).Scan(&id)

	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("insert payment: %w", err)
	}

	if in.Result != ResultApplied {
		return true, nil
	}

	if in.NewDeploymentState != "" && in.NewDeploymentState != DeploymentActive {
		_, err = q.Exec(ctx, `
			UPDATE deployments
			SET current_balance_kobo = $2,
			    state = $3::deployment_state,
			    closed_at = now()
			WHERE id = $1
		`, *in.DeploymentID, in.AppliedBalanceKobo.Int64(), string(in.NewDeploymentState))
	} else {
		_, err = q.Exec(ctx, `
			UPDATE deployments
			SET current_balance_kobo = $2
			WHERE id = $1
		`, *in.DeploymentID, in.AppliedBalanceKobo.Int64())
	}
	if err != nil {
		return true, fmt.Errorf("update deployment: %w", err)
	}
	return true, nil
}

func validateRecordIn(in RecordIn) error {
	switch in.Result {
	case ResultApplied:
		if in.DeploymentID == nil || in.AppliedBalanceKobo == nil {
			return fmt.Errorf("record: APPLIED requires deployment id and applied balance")
		}
	case ResultRejected:
		if in.RejectReason == "" {
			return fmt.Errorf("record: REJECTED requires reject reason")
		}
	case ResultRecorded:
		// no additional fields required
	default:
		return fmt.Errorf("record: unknown result %q", in.Result)
	}
	if len(in.ResponseBody) == 0 {
		return fmt.Errorf("record: response body is required")
	}
	if in.ResponseStatus < 100 || in.ResponseStatus > 599 {
		return fmt.Errorf("record: response status %d out of range", in.ResponseStatus)
	}
	return nil
}
