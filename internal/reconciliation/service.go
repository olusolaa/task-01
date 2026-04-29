// Package reconciliation exposes a read path that both serves the stored
// balance and recomputes it from the payment ledger, surfacing any drift.
//
// The stored column deployments.current_balance_kobo is a cache of the
// projection value_kobo - SUM(applied payments). In a correct system the
// cache and the projection are always equal; any non-zero drift is a data
// integrity incident and is exposed in the response rather than papered over.
package reconciliation

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/olusolaa/paybook/internal/money"
)

var ErrCustomerNotFound = errors.New("customer_not_found")

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

type CustomerBalance struct {
	CustomerID  string              `json:"customer_id"`
	Deployments []DeploymentBalance `json:"deployments"`
}

type DeploymentBalance struct {
	DeploymentID         string `json:"deployment_id"`
	State                string `json:"state"`
	ValueNaira           string `json:"value_naira"`
	StoredBalanceNaira   string `json:"stored_balance_naira"`
	ComputedBalanceNaira string `json:"computed_balance_naira"`
	DriftNaira           string `json:"drift_naira"`
}

func (s *Service) ForCustomer(ctx context.Context, customerID string) (*CustomerBalance, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM customers WHERE id = $1)`,
		customerID,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check customer: %w", err)
	}
	if !exists {
		return nil, ErrCustomerNotFound
	}

	rows, err := s.pool.Query(ctx, `
		SELECT
			d.id,
			d.state::text,
			d.value_kobo,
			d.current_balance_kobo,
			d.value_kobo - COALESCE(SUM(p.amount_kobo) FILTER (WHERE p.result = 'APPLIED'), 0) AS computed
		FROM deployments d
		LEFT JOIN payments p ON p.deployment_id = d.id
		WHERE d.customer_id = $1
		GROUP BY d.id
		ORDER BY d.started_at ASC
	`, customerID)
	if err != nil {
		return nil, fmt.Errorf("query deployments: %w", err)
	}
	defer rows.Close()

	out := &CustomerBalance{CustomerID: customerID}
	for rows.Next() {
		var (
			depID, state                 string
			valueKobo, storedKobo, compKobo int64
		)
		if err := rows.Scan(&depID, &state, &valueKobo, &storedKobo, &compKobo); err != nil {
			return nil, fmt.Errorf("scan deployment: %w", err)
		}
		out.Deployments = append(out.Deployments, DeploymentBalance{
			DeploymentID:         depID,
			State:                state,
			ValueNaira:           money.Kobo(valueKobo).Naira(),
			StoredBalanceNaira:   money.Kobo(storedKobo).Naira(),
			ComputedBalanceNaira: money.Kobo(compKobo).Naira(),
			DriftNaira:           money.Kobo(storedKobo - compKobo).Naira(),
		})
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("scan rows: %w", err)
	}
	return out, nil
}
