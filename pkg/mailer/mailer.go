package mailer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// DefaultRecipientsFile is the default file name for recipients.
const DefaultRecipientsFile = "recipients.txt"

const (
	// DefaultEmailSubject is the subject line used if none is provided.
	DefaultEmailSubject = "Software Developer (SDE 1) opportunity"

	// DefaultEmailBody is the body text used if none is provided.
	DefaultEmailBody = `Hi,

I hope you're doing well. I'm Kushal Jangra, currently working as a Software Developer at Omniful, and I'm reaching out to explore Software Developer (SDE 1) opportunities at your organization.

I have hands-on experience in backend development using Golang, where I've worked on building scalable microservices and REST APIs, along with a solid understanding of distributed systems, databases, and system design principles. I've also worked with technologies like PostgreSQL, Kafka, and AWS, and have experience in developing reliable and production-ready systems.

I'm particularly interested in backend engineering and enjoy solving complex problems while building efficient and scalable systems. I would love the opportunity to contribute and grow within your team.

Please find my resume attached for your review. I look forward to hearing from you.

Best regards,
Kushal Jangra
+91-9817747251`
)

// AppDir returns the directory containing the executable.
func AppDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

// LoadDotEnv loads .env from the same folder as the executable (works when double-clicking),
// then falls back to the current working directory.
func LoadDotEnv() error {
	exeDir, err := AppDir()
	if err != nil {
		return err
	}
	candidates := []string{filepath.Join(exeDir, ".env")}
	if wd, err := os.Getwd(); err == nil {
		wdp := filepath.Join(wd, ".env")
		if wdp != candidates[0] {
			candidates = append(candidates, wdp)
		}
	}
	for _, p := range candidates {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		if err := godotenv.Overload(p); err != nil {
			return fmt.Errorf("load %s: %w", p, err)
		}
		return nil
	}
	return fmt.Errorf(".env not found — place .env in the same folder as this program (%s)", exeDir)
}

// ResolveBesideApp resolves a path relative to the given base directory.
func ResolveBesideApp(baseDir, p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(baseDir, p)
}

// SmtpConfigFromEnv loads standard SMTP environment variables from .env.
func SmtpConfigFromEnv() (host string, port int, user, pass, from string, useOAuth2 bool, err error) {
	if err = LoadDotEnv(); err != nil {
		return "", 0, "", "", "", false, err
	}
	return SmtpConfigFromCurrentEnv()
}

// SmtpConfigFromCurrentEnv reads SMTP settings from the current process environment (after LoadDotEnv / WithEnv).
func SmtpConfigFromCurrentEnv() (host string, port int, user, pass, from string, useOAuth2 bool, err error) {
	host = strings.TrimSpace(os.Getenv("SMTP_HOST"))
	user = strings.TrimSpace(os.Getenv("SMTP_USER"))
	pass = strings.TrimSpace(os.Getenv("SMTP_PASSWORD"))
	from = strings.TrimSpace(os.Getenv("FROM_EMAIL"))
	if from == "" {
		from = user
	}
	useOAuth2 = strings.EqualFold(strings.TrimSpace(os.Getenv("SMTP_USE_OAUTH2")), "true")
	port = 587
	if p := strings.TrimSpace(os.Getenv("SMTP_PORT")); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil {
			return "", 0, "", "", "", false, fmt.Errorf("SMTP_PORT: %w", err)
		}
	}
	if host == "" || user == "" {
		return "", 0, "", "", "", false, fmt.Errorf("set SMTP_HOST and SMTP_USER in .env or in the request")
	}
	if !useOAuth2 && pass == "" {
		return "", 0, "", "", "", false, fmt.Errorf("set SMTP_PASSWORD, or SMTP_USE_OAUTH2=true with GOOGLE_OAUTH_* vars")
	}
	return host, port, user, pass, from, useOAuth2, nil
}

// SendParams holds one outbound send (CLI or HTTP).
type SendParams struct {
	Recipients []string
	Subject    string
	Body       string
	BodyFile   string
	At         string
	DelaySec   *int
	UseSSL     bool
	NoAttach   bool
	AttachPath string
}

// WithEnv runs a function with overridden environment variables.
func WithEnv(overlay map[string]string, fn func() error) error {
	if len(overlay) == 0 {
		return fn()
	}
	keys := make([]string, 0, len(overlay))
	old := make(map[string]string, len(overlay))
	for k, v := range overlay {
		keys = append(keys, k)
		old[k] = os.Getenv(k)
		os.Setenv(k, v)
	}
	defer func() {
		for _, k := range keys {
			if old[k] == "" {
				_ = os.Unsetenv(k)
			} else {
				os.Setenv(k, old[k])
			}
		}
	}()
	return fn()
}

// DoSend coordinates sending the email with the given parameters and attachments.
func DoSend(ctx context.Context, exeDir string, smtpHost string, smtpPort int, user, pass, from string, useOAuth2 bool, p SendParams) error {
	list := UniqPreserve(p.Recipients)
	if len(list) == 0 {
		return fmt.Errorf("no recipients")
	}

	subjectLine := strings.TrimSpace(p.Subject)
	if subjectLine == "" {
		subjectLine = DefaultEmailSubject
	}

	bodyText := DefaultEmailBody
	if strings.TrimSpace(p.Body) != "" {
		bodyText = p.Body
	}
	if strings.TrimSpace(p.BodyFile) != "" {
		bodyPath := ResolveBesideApp(exeDir, p.BodyFile)
		b, err := os.ReadFile(bodyPath)
		if err != nil {
			return fmt.Errorf("body file: %w", err)
		}
		bodyText = string(b)
	}

	var delayPtr *int
	if p.DelaySec != nil && *p.DelaySec >= 0 {
		delayPtr = p.DelaySec
	}
	atStr := strings.TrimSpace(p.At)
	if atStr != "" && delayPtr != nil {
		return fmt.Errorf("use only one of At schedule or DelaySec")
	}
	if err := waitSchedule(atStr, delayPtr); err != nil {
		return err
	}

	attachPath := p.AttachPath
	if attachPath == "" {
		attachPath = "Kushal_resume_Backend.pdf"
	}
	attachPath = ResolveBesideApp(exeDir, attachPath)

	implicitTLS := p.UseSSL || smtpPort == 465
	var msg []byte
	var err error
	if p.NoAttach {
		msg = buildMessage(from, list, subjectLine, bodyText)
	} else {
		msg, err = buildMessageWithPDF(from, list, subjectLine, bodyText, attachPath)
		if err != nil {
			return err
		}
	}

	auth, err := smtpAuth(ctx, smtpHost, user, pass, useOAuth2)
	if err != nil {
		return fmt.Errorf("smtp auth: %w", err)
	}

	if err = sendMail(smtpHost, smtpPort, auth, from, list, msg, implicitTLS); err != nil {
		return fmt.Errorf("send failed: %w", err)
	}
	return nil
}

// xoauth2Auth is Gmail SMTP OAuth2.
type xoauth2Auth struct {
	email, accessToken string
}

func (a xoauth2Auth) Start(*smtp.ServerInfo) (string, []byte, error) {
	s := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", a.email, a.accessToken)
	return "XOAUTH2", []byte(s), nil
}

func (a xoauth2Auth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("xoauth2: %s", string(fromServer))
	}
	return nil, nil
}

// LoadGoogleOAuthCreds loads client credentials to refresh Google tokens.
func LoadGoogleOAuthCreds() (clientID, clientSecret, refresh string, err error) {
	clientID = strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_CLIENT_ID"))
	clientSecret = strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"))
	refresh = strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_REFRESH_TOKEN"))
	if clientID == "" || clientSecret == "" || refresh == "" {
		return "", "", "", fmt.Errorf("set GOOGLE_OAUTH_CLIENT_ID, GOOGLE_OAUTH_CLIENT_SECRET, GOOGLE_OAUTH_REFRESH_TOKEN")
	}
	return clientID, clientSecret, refresh, nil
}

func accessTokenFromRefresh(ctx context.Context, clientID, clientSecret, refresh string) (string, error) {
	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{"https://mail.google.com/"},
	}
	tok, err := cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refresh}).Token()
	if err != nil {
		return "", fmt.Errorf("google token refresh: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("empty access token from google")
	}
	return tok.AccessToken, nil
}

func smtpAuth(ctx context.Context, host, user, pass string, useOAuth2 bool) (smtp.Auth, error) {
	if useOAuth2 {
		id, secret, refresh, err := LoadGoogleOAuthCreds()
		if err != nil {
			return nil, err
		}
		at, err := accessTokenFromRefresh(ctx, id, secret, refresh)
		if err != nil {
			return nil, err
		}
		return xoauth2Auth{email: user, accessToken: at}, nil
	}
	return smtp.PlainAuth("", user, pass, host), nil
}

var emailLine = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)

// LoadRecipients loads recipient addresses from the specified file.
func LoadRecipients(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if emailLine.MatchString(line) {
			out = append(out, line)
		} else {
			fmt.Fprintf(os.Stderr, "warning: skipping invalid line: %q\n", line)
		}
	}
	return out, s.Err()
}

// UniqPreserve removes duplicate strings while preserving order.
func UniqPreserve(emails []string) []string {
	seen := make(map[string]struct{}, len(emails))
	var out []string
	for _, e := range emails {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

func encodeSubject(s string) string {
	if isASCII(s) {
		return s
	}
	return fmt.Sprintf("=?UTF-8?B?%s?=", base64.StdEncoding.EncodeToString([]byte(s)))
}

func buildMessage(from string, to []string, subject, body string) []byte {
	var b bytes.Buffer
	writeCommonHeaders(&b, from, to, subject)
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	writeBodyCRLF(&b, body)
	return b.Bytes()
}

func writeCommonHeaders(b *bytes.Buffer, from string, to []string, subject string) {
	fmt.Fprintf(b, "From: %s\r\n", from)
	fmt.Fprintf(b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(b, "Subject: %s\r\n", encodeSubject(subject))
	b.WriteString("MIME-Version: 1.0\r\n")
}

func writeBodyCRLF(w io.Writer, body string) {
	s := strings.ReplaceAll(body, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	io.WriteString(w, strings.ReplaceAll(s, "\n", "\r\n"))
	if !strings.HasSuffix(s, "\n") && s != "" {
		io.WriteString(w, "\r\n")
	}
}

func writeBase64Body(w io.Writer, raw []byte) {
	enc := base64.StdEncoding.EncodeToString(raw)
	const line = 76
	for i := 0; i < len(enc); i += line {
		j := i + line
		if j > len(enc) {
			j = len(enc)
		}
		w.Write([]byte(enc[i:j]))
		w.Write([]byte("\r\n"))
	}
}

func buildMessageWithPDF(from string, to []string, subject, body, pdfPath string) ([]byte, error) {
	pdfData, err := os.ReadFile(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("read PDF %q: %w", pdfPath, err)
	}

	var mimeBody bytes.Buffer
	mw := multipart.NewWriter(&mimeBody)
	boundary := mw.Boundary()

	hText := textproto.MIMEHeader{}
	hText.Set("Content-Type", "text/plain; charset=UTF-8")
	hText.Set("Content-Transfer-Encoding", "8bit")
	pw, err := mw.CreatePart(hText)
	if err != nil {
		return nil, err
	}
	writeBodyCRLF(pw, body)

	fn := filepath.Base(pdfPath)
	hPDF := textproto.MIMEHeader{}
	hPDF.Set("Content-Type", fmt.Sprintf(`application/pdf; name=%q`, fn))
	hPDF.Set("Content-Transfer-Encoding", "base64")
	hPDF.Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, fn))
	pdfPart, err := mw.CreatePart(hPDF)
	if err != nil {
		return nil, err
	}
	writeBase64Body(pdfPart, pdfData)
	if err = mw.Close(); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	writeCommonHeaders(&out, from, to, subject)
	fmt.Fprintf(&out, "Content-Type: multipart/mixed; boundary=%s\r\n\r\n", boundary)
	out.Write(mimeBody.Bytes())
	return out.Bytes(), nil
}

func parseAtLocalTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-1-02 15:04:05",
		"2006-01-2 15:04:05",
		"2006-1-2 15:04:05",
	}
	var lastErr error
	for _, layout := range layouts {
		t, err := time.ParseInLocation(layout, s, time.Local)
		if err == nil {
			return t, nil
		}
		lastErr = err
	}
	return time.Time{}, lastErr
}

func waitSchedule(atStr string, delaySec *int) error {
	if delaySec != nil && *delaySec > 0 {
		fmt.Printf("Waiting %d seconds before send...\n", *delaySec)
		time.Sleep(time.Duration(*delaySec) * time.Second)
		return nil
	}
	if atStr == "" {
		return nil
	}
	target, err := parseAtLocalTime(atStr)
	if err != nil {
		return fmt.Errorf("--at: %w", err)
	}
	now := time.Now()
	if !target.After(now) {
		fmt.Fprintln(os.Stderr, "Scheduled time is in the past; sending now.")
		return nil
	}
	d := target.Sub(now)
	fmt.Printf("Scheduled send at %s (in %d seconds)...\n", target.Format(time.RFC3339), int(d.Seconds()))
	time.Sleep(d)
	return nil
}

func sendMail(host string, port int, auth smtp.Auth, from string, to []string, msg []byte, useSSL bool) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	if useSSL {
		tlsCfg := &tls.Config{ServerName: host}
		conn, err := tls.Dial("tcp", addr, tlsCfg)
		if err != nil {
			return err
		}
		defer conn.Close()
		c, err := smtp.NewClient(conn, host)
		if err != nil {
			return err
		}
		defer c.Close()
		if err = c.Auth(auth); err != nil {
			return err
		}
		if err = c.Mail(from); err != nil {
			return err
		}
		for _, rcpt := range to {
			if err = c.Rcpt(rcpt); err != nil {
				return err
			}
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		if _, err = w.Write(msg); err != nil {
			_ = w.Close()
			return err
		}
		return w.Close()
	}

	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err = c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if err = c.Auth(auth); err != nil {
		return err
	}
	if err = c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err = c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err = io.Copy(w, bytes.NewReader(msg)); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// SendHTTPRequest represents the expected JSON payload for HTTP send requests.
type SendHTTPRequest struct {
	Env          map[string]string `json:"env"`
	Recipients   []string          `json:"recipients"`
	Subject      string            `json:"subject"`
	Body         string            `json:"body"`
	BodyFile     string            `json:"bodyFile"`
	At           string            `json:"at"`
	DelaySeconds *int              `json:"delaySeconds"`
	UseSSL       bool              `json:"useSSL"`
	NoAttach     bool              `json:"noAttach"`
	Attach       string            `json:"attach"`
}
