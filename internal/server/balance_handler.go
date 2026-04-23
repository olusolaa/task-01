package server

import (
	"errors"
	"net/http"

	"github.com/olusolaa/paybook/internal/reconciliation"
)

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	customerID := r.PathValue("id")
	if customerID == "" {
		writeError(w, http.StatusBadRequest, "missing_id", "")
		return
	}
	out, err := s.reconciliation.ForCustomer(r.Context(), customerID)
	if err != nil {
		if errors.Is(err, reconciliation.ErrCustomerNotFound) {
			writeError(w, http.StatusNotFound, "customer_not_found", "")
			return
		}
		s.log.Error("balance query failed",
			"err", err,
			"request_id", RequestIDFromContext(r.Context()),
		)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
