package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"sendemails/pkg/mailer"
)

// Handler handles email send requests on Vercel.
func Handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req mailer.SendHTTPRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	rec := mailer.UniqPreserve(req.Recipients)
	if len(rec) == 0 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "add at least one recipient email"})
		return
	}

	var delay *int
	if req.DelaySeconds != nil && *req.DelaySeconds >= 0 {
		delay = req.DelaySeconds
	}
	at := strings.TrimSpace(req.At)
	if at != "" && delay != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "use only one of at or delaySeconds"})
		return
	}

	params := mailer.SendParams{
		Recipients: rec,
		Subject:    req.Subject,
		Body:       req.Body,
		BodyFile:   req.BodyFile,
		At:         at,
		DelaySec:   delay,
		UseSSL:     req.UseSSL,
		NoAttach:   req.NoAttach,
		AttachPath: req.Attach,
	}

	if params.AttachPath == "" {
		params.AttachPath = "Kushal_resume_Backend.pdf"
	}

	// Use current working directory for asset resolution on Vercel.
	exeDir := "."

	sendErr := mailer.WithEnv(req.Env, func() error {
		smtpHost, smtpPort, user, pass, from, useOAuth2, err2 := mailer.SmtpConfigFromCurrentEnv()
		if err2 != nil {
			return err2
		}
		return mailer.DoSend(context.Background(), exeDir, smtpHost, smtpPort, user, pass, from, useOAuth2, params)
	})

	if sendErr != nil {
		code := http.StatusBadRequest
		if strings.HasPrefix(sendErr.Error(), "send failed:") || strings.HasPrefix(sendErr.Error(), "smtp auth:") {
			code = http.StatusBadGateway
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": sendErr.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "sent": len(rec)})
}
