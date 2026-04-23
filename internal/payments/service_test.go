package payments

import (
	"errors"
	"testing"
	"time"

	"github.com/olusolaa/paybook/internal/money"
)

type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }

func newTestService() *Service {
	s := &Service{clockSkewGrace: 24 * time.Hour}
	s.clock = fixedClock{t: time.Date(2025, 11, 7, 15, 0, 0, 0, time.UTC)}
	return s
}

func TestValidateAndParse_Happy(t *testing.T) {
	s := newTestService()
	in := RawInput{
		CustomerID:           "GIG00001",
		PaymentStatus:        "COMPLETE",
		TransactionAmount:    "10000",
		TransactionDate:      "2025-11-07 14:54:16",
		TransactionReference: "VPAY1",
	}
	p, err := s.ValidateAndParse(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.AmountKobo != money.Kobo(10000) {
		t.Fatalf("amount: got %d", p.AmountKobo)
	}
	if p.Status != StatusComplete {
		t.Fatalf("status: got %s", p.Status)
	}
}

func TestValidateAndParse_FutureDateRejected(t *testing.T) {
	s := newTestService()
	in := RawInput{
		CustomerID:           "GIG00001",
		PaymentStatus:        "COMPLETE",
		TransactionAmount:    "10000",
		TransactionDate:      "2025-11-09 14:54:16",
		TransactionReference: "VPAY2",
	}
	_, err := s.ValidateAndParse(in)
	if !errors.Is(err, ErrInvalidDate) {
		t.Fatalf("expected invalid date, got %v", err)
	}
}

func TestValidateAndParse_BadStatus(t *testing.T) {
	s := newTestService()
	in := RawInput{
		CustomerID:           "GIG00001",
		PaymentStatus:        "SUCCESS",
		TransactionAmount:    "10000",
		TransactionDate:      "2025-11-07 14:54:16",
		TransactionReference: "VPAY3",
	}
	_, err := s.ValidateAndParse(in)
	if !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("expected invalid status, got %v", err)
	}
}

func TestDecide_Applied(t *testing.T) {
	p := Payment{
		CustomerID:           "GIG00001",
		TransactionReference: "VPAY1",
		AmountKobo:           money.Kobo(10000),
		Status:               StatusComplete,
	}
	dep := &Deployment{
		ID:                 "11111111-1111-1111-1111-111111111111",
		CustomerID:         "GIG00001",
		ValueKobo:          money.Kobo(100000000),
		CurrentBalanceKobo: money.Kobo(100000000),
		State:              DeploymentActive,
	}
	d := decide(p, dep)
	if d.result != ResultApplied {
		t.Fatalf("result: %s", d.result)
	}
	if d.status != 201 {
		t.Fatalf("status: %d", d.status)
	}
	if d.newBalance == nil || *d.newBalance != money.Kobo(100000000-10000) {
		t.Fatalf("new balance: %v", d.newBalance)
	}
	if d.newDeploymentState != DeploymentActive {
		t.Fatalf("state: %s", d.newDeploymentState)
	}
}

func TestDecide_TransitionsOnZero(t *testing.T) {
	p := Payment{Status: StatusComplete, AmountKobo: 100, TransactionReference: "x"}
	dep := &Deployment{State: DeploymentActive, CurrentBalanceKobo: 100, ValueKobo: 1000, ID: "x"}
	d := decide(p, dep)
	if d.newDeploymentState != DeploymentFullyRepaid {
		t.Fatalf("expected FULLY_REPAID, got %s", d.newDeploymentState)
	}
	if d.newBalance == nil || *d.newBalance != 0 {
		t.Fatalf("expected zero balance, got %v", d.newBalance)
	}
}

func TestDecide_Overpayment(t *testing.T) {
	p := Payment{Status: StatusComplete, AmountKobo: 200, TransactionReference: "x"}
	dep := &Deployment{State: DeploymentActive, CurrentBalanceKobo: 100, ValueKobo: 1000, ID: "x"}
	d := decide(p, dep)
	if d.result != ResultRejected || d.rejectReason != "overpayment" {
		t.Fatalf("got %+v", d)
	}
	if d.status != 409 {
		t.Fatalf("status: %d", d.status)
	}
}

func TestDecide_InactiveDeployment(t *testing.T) {
	p := Payment{Status: StatusComplete, AmountKobo: 10, TransactionReference: "x"}
	dep := &Deployment{State: DeploymentFullyRepaid, CurrentBalanceKobo: 0, ValueKobo: 1000, ID: "x"}
	d := decide(p, dep)
	if d.result != ResultRejected || d.rejectReason != "deployment_inactive" {
		t.Fatalf("got %+v", d)
	}
}

func TestDecide_NonCompleteRecorded(t *testing.T) {
	p := Payment{Status: StatusPending, AmountKobo: 10, TransactionReference: "x"}
	dep := &Deployment{State: DeploymentActive, CurrentBalanceKobo: 1000, ValueKobo: 1000, ID: "x"}
	d := decide(p, dep)
	if d.result != ResultRecorded {
		t.Fatalf("got %s", d.result)
	}
	if d.status != 202 {
		t.Fatalf("status: %d", d.status)
	}
	if d.newBalance != nil {
		t.Fatal("recorded must not touch balance")
	}
}
