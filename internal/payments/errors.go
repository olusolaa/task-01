package payments

import "errors"

// Sentinel errors returned by the service and mapped to HTTP status by the
// handler. The mapping is the one place this taxonomy is interpreted.
var (
	ErrCustomerNotFound   = errors.New("customer_not_found")
	ErrDeploymentNotFound = errors.New("no_active_deployment")
	ErrInvalidAmount      = errors.New("invalid_amount")
	ErrInvalidDate        = errors.New("invalid_date")
	ErrInvalidStatus      = errors.New("invalid_status")
	ErrInvalidPayload     = errors.New("invalid_payload")
)
