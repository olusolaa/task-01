package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/olusolaa/paybook/internal/payments"
)

type applyRequest struct {
	CustomerID           string `json:"customer_id"`
	PaymentStatus        string `json:"payment_status"`
	TransactionAmount    string `json:"transaction_amount"`
	TransactionDate      string `json:"transaction_date"`
	TransactionReference string `json:"transaction_reference"`
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var req applyRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	p, err := s.payments.ValidateAndParse(payments.RawInput{
		CustomerID:           req.CustomerID,
		PaymentStatus:        req.PaymentStatus,
		TransactionAmount:    req.TransactionAmount,
		TransactionDate:      req.TransactionDate,
		TransactionReference: req.TransactionReference,
	})
	if err != nil {
		s.writeValidationError(w, err)
		return
	}

	out, err := s.payments.Apply(r.Context(), p)
	if err != nil {
		s.writeApplyError(r, w, err)
		return
	}

	if out.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		s.metrics.ObservePayment("REPLAYED")
	} else {
		s.metrics.ObservePayment(string(out.Result))
	}
	writeBytes(w, out.Status, out.Body)
}

func (s *Server) writeValidationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, payments.ErrInvalidAmount):
		writeError(w, http.StatusBadRequest, "invalid_amount", err.Error())
	case errors.Is(err, payments.ErrInvalidDate):
		writeError(w, http.StatusBadRequest, "invalid_date", err.Error())
	case errors.Is(err, payments.ErrInvalidStatus):
		writeError(w, http.StatusBadRequest, "invalid_status", err.Error())
	case errors.Is(err, payments.ErrInvalidPayload):
		writeError(w, http.StatusBadRequest, "invalid_payload", err.Error())
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	}
}

func (s *Server) writeApplyError(r *http.Request, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, payments.ErrCustomerNotFound):
		writeError(w, http.StatusNotFound, "customer_not_found", "")
	case errors.Is(err, payments.ErrDeploymentNotFound):
		writeError(w, http.StatusNotFound, "no_active_deployment", "")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, "cancelled", "")
	default:
		s.log.Error("apply failed",
			"err", err,
			"request_id", RequestIDFromContext(r.Context()),
		)
		writeError(w, http.StatusInternalServerError, "internal", "")
	}
}
