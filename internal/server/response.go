package server

import (
	"encoding/json"
	"net/http"
)

type errorBody struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

// writeJSON serialises v and writes it with the given status.
// The content-type header is set before WriteHeader so it applies to the body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"marshal"}`))
		return
	}
	writeBytes(w, status, b)
}

// writeBytes writes pre-marshaled JSON bytes.
func writeBytes(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, errorBody{Error: code, Detail: detail})
}
