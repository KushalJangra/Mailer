package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"sendemails/pkg/mailer"
)


//go:embed public
var webFS embed.FS

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Handler handles all API and UI requests for Vercel and local server.
func Handler(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		// Serve UI from embedded FS
		sub, err := fs.Sub(webFS, "public")
		if err != nil {
			http.Error(w, "FS error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		http.FileServer(http.FS(sub)).ServeHTTP(w, r)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api")
	path = strings.TrimPrefix(path, "/")

	switch path {
	case "health":
		handleHealth(w, r)
	case "send":
		handleSend(w, r)
	default:
		http.NotFound(w, r)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req mailer.SendHTTPRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	rec := mailer.UniqPreserve(req.Recipients)
	if len(rec) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "add at least one recipient email"})
		return
	}

	var delay *int
	if req.DelaySeconds != nil && *req.DelaySeconds >= 0 {
		delay = req.DelaySeconds
	}
	at := strings.TrimSpace(req.At)
	if at != "" && delay != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "use only one of at or delaySeconds"})
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

	exeDir, err := mailer.AppDir()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	_ = mailer.LoadDotEnv()
	sendErr := mailer.WithEnv(req.Env, func() error {
		smtpHost, smtpPort, user, pass, from, useOAuth2, err2 := mailer.SmtpConfigFromCurrentEnv()
		if err2 != nil {
			return err2
		}
		return mailer.DoSend(r.Context(), exeDir, smtpHost, smtpPort, user, pass, from, useOAuth2, params)
	})

	if sendErr != nil {
		code := http.StatusBadRequest
		if strings.HasPrefix(sendErr.Error(), "send failed:") || strings.HasPrefix(sendErr.Error(), "smtp auth:") {
			code = http.StatusBadGateway
		}
		writeJSON(w, code, map[string]string{"error": sendErr.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "sent": len(rec)})
}

func runHTTPServer(addr string) {
	sub, err := fs.Sub(webFS, "public")
	if err != nil {
		log.Fatal(err)
	}
	fileSrv := http.FileServer(http.FS(sub))
	mux := http.NewServeMux()

	// Use the unified Handler for all /api/ requests
	mux.HandleFunc("/api/", Handler)

	mux.Handle("/", fileSrv)
	listenAddr := addr
	if listenAddr != "" && !strings.Contains(listenAddr, ":") {
		listenAddr = ":" + listenAddr
	}
	log.Printf("web UI: http://127.0.0.1%s/\n", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}

func main() {
	var toFlags stringList
	flag.Var(&toFlags, "to", "Recipient email (repeatable)")
	recipientsFile := flag.String("recipients", "", fmt.Sprintf("Recipients file (default beside .exe: %s when -to not used)", mailer.DefaultRecipientsFile))
	subject := flag.String("subject", "", "Message subject (uses built-in default if empty)")
	body := flag.String("body", "", "Plain text body (uses built-in default if empty and no -body-file)")
	bodyFile := flag.String("body-file", "", "Read body from UTF-8 file (overrides -body)")
	at := flag.String("at", "", `Optional: wait until local time (e.g. 2026-03-28 23:50:00) then send; runs only on this PC`)
	delaySec := flag.Int("delay-seconds", -1, "Wait N seconds before send (use -1 to disable)")
	useSSL := flag.Bool("ssl", false, "Use implicit TLS (typical for port 465)")
	verifyEnv := flag.Bool("verify-env", false, "Print effective SMTP settings from .env (password not shown) and exit")
	noAttach := flag.Bool("no-attach", false, "Send without PDF attachment")
	attachPath := flag.String("attach", "Kushal_resume_Backend.pdf", "Path to PDF file to attach")
	httpAddr := flag.String("http", "", "Listen address for web UI and API (e.g. :8080 or 8080); if set, runs server instead of CLI send")
	flag.Parse()

	if *httpAddr != "" {
		runHTTPServer(*httpAddr)
		return
	}

	// For Vercel: Automatically run the server if the PORT environment variable is set
	if port := os.Getenv("PORT"); port != "" {
		log.Printf("Detected Vercel/environment PORT=%s, starting server...\n", port)
		runHTTPServer(port)
		return
	}

	exeDir, err := mailer.AppDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "app path: %v\n", err)
		os.Exit(1)
	}
	attachRel := *attachPath

	if *at != "" && *delaySec >= 0 {
		fmt.Fprintln(os.Stderr, "use only one of -at or -delay-seconds")
		os.Exit(1)
	}

	smtpHost, smtpPort, user, pass, from, useOAuth2, err := mailer.SmtpConfigFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if *verifyEnv {
		fmt.Printf("SMTP_HOST=%s\nSMTP_PORT=%d\nSMTP_USER=%s\nFROM_EMAIL=%s\n", smtpHost, smtpPort, user, from)
		fmt.Printf("SMTP_USE_OAUTH2=%v\n", useOAuth2)
		if useOAuth2 {
			_, _, _, oerr := mailer.LoadGoogleOAuthCreds()
			if oerr != nil {
				fmt.Fprintln(os.Stderr, oerr)
			} else {
				fmt.Println("GOOGLE_OAUTH_*: client id, secret, and refresh token are set (values hidden)")
			}
		} else {
			fmt.Printf("SMTP_PASSWORD length=%d (Gmail app passwords are 16 characters, no spaces)\n", len(pass))
			if len(pass) != 16 && pass != "" {
				fmt.Fprintln(os.Stderr, "warning: password length is not 16; typo or wrong secret type? If sure it is correct, try SMTP_USE_OAUTH2=true instead.")
			}
		}
		os.Exit(0)
	}

	recPath := *recipientsFile
	if recPath == "" && len(toFlags) == 0 {
		recPath = filepath.Join(exeDir, mailer.DefaultRecipientsFile)
	} else if recPath != "" {
		recPath = mailer.ResolveBesideApp(exeDir, recPath)
	}

	var list []string
	if recPath != "" {
		list, err = mailer.LoadRecipients(recPath)
		if err != nil {
			if *recipientsFile == "" && len(toFlags) == 0 {
				fmt.Fprintf(os.Stderr, "Create %s beside this program with one email per line, or use -to you@example.com\n", filepath.Join(exeDir, mailer.DefaultRecipientsFile))
			}
			fmt.Fprintf(os.Stderr, "recipients: %v\n", err)
			os.Exit(1)
		}
	}
	list = append(list, toFlags...)
	list = mailer.UniqPreserve(list)
	if len(list) == 0 {
		fmt.Fprintf(os.Stderr, "no recipients: edit %s or use -to / -recipients\n", filepath.Join(exeDir, mailer.DefaultRecipientsFile))
		os.Exit(1)
	}

	var delayPtr *int
	if *delaySec >= 0 {
		delayPtr = delaySec
	}
	params := mailer.SendParams{
		Recipients: list,
		Subject:    *subject,
		Body:       *body,
		BodyFile:   *bodyFile,
		At:         *at,
		DelaySec:   delayPtr,
		UseSSL:     *useSSL,
		NoAttach:   *noAttach,
		AttachPath: attachRel,
	}
	if err := mailer.DoSend(context.Background(), exeDir, smtpHost, smtpPort, user, pass, from, useOAuth2, params); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		if !useOAuth2 && strings.Contains(err.Error(), "535") {
			fmt.Fprintln(os.Stderr, "If the app password is correct: Google may still reject this login. Regenerate the app password, confirm 2-Step Verification, or set SMTP_USE_OAUTH2=true (see .env.example).")
		}
		os.Exit(1)
	}
	fmt.Printf("Sent to %d recipient(s).\n", len(list))
}

type stringList []string

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func (s *stringList) String() string { return strings.Join(*s, ",") }
