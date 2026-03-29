// Package health provides the Vercel serverless health endpoint.
package health

import (
	"encoding/json"
	"net/http"
)

// Handler handles health check requests on Vercel.
func Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
