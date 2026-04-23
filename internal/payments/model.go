package payments

import (
	"time"

	"github.com/olusolaa/paybook/internal/money"
)

// Status mirrors the payment_status enum in Postgres. Parse with ParseStatus
// so unknown values are rejected at the boundary, not discovered later.
type Status string

const (
	StatusComplete Status = "COMPLETE"
	StatusPending  Status = "PENDING"
	StatusFailed   Status = "FAILED"
	StatusReversed Status = "REVERSED"
)

func ParseStatus(s string) (Status, error) {
	switch Status(s) {
	case StatusComplete, StatusPending, StatusFailed, StatusReversed:
		return Status(s), nil
	}
	return "", ErrInvalidStatus
}

// Result records what the service did with a payment. It is written to the
// ledger row so the outcome is explicit in the data.
type Result string

const (
	ResultApplied  Result = "APPLIED"  // money moved, balance decremented
	ResultRecorded Result = "RECORDED" // kept for audit, balance untouched
	ResultRejected Result = "REJECTED" // refused (overpayment, inactive deployment)
)

// DeploymentState mirrors the deployment_state enum.
type DeploymentState string

const (
	DeploymentActive      DeploymentState = "ACTIVE"
	DeploymentFullyRepaid DeploymentState = "FULLY_REPAID"
	DeploymentDefaulted   DeploymentState = "DEFAULTED"
	DeploymentWrittenOff  DeploymentState = "WRITTEN_OFF"
)

// Payment is a validated, parsed notification ready to be applied.
// Construction happens in the service layer; repo and handler accept it as
// already-valid domain input.
type Payment struct {
	CustomerID           string
	TransactionReference string
	AmountKobo           money.Kobo
	Status               Status
	TransactionDate      time.Time
}

// Deployment is the minimal projection of a deployment row that the service
// needs to decide an outcome. The full row has more fields; we only read
// what we use.
type Deployment struct {
	ID                 string
	CustomerID         string
	CurrentBalanceKobo money.Kobo
	State              DeploymentState
	StartedAt          time.Time
}

// Outcome is the response the service produced for a given payment. Status
// and Body are the exact bytes served to the client; they are also written
// to the ledger row so a replay returns them verbatim.
type Outcome struct {
	Replayed bool
	Result   Result
	Status   int
	Body     []byte
}
