package serve

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// sessionCookieName is the browser cookie holding the session token.
	sessionCookieName = "tern_session"
	// sessionTokenTTL bounds how long a browser stays logged in.
	sessionTokenTTL = 24 * time.Hour
	// passwordAlphabet avoids ambiguous characters so a generated password
	// can be read off a terminal and typed on a phone.
	passwordAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"
)

// GeneratePassword returns a strong, human-readable random password in the
// form "abcd-efgh-ijkl-mnop" (16 characters from a 32-symbol alphabet, about
// 80 bits of entropy).
func GeneratePassword() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, v := range buf {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(passwordAlphabet[int(v)%len(passwordAlphabet)])
	}
	return b.String(), nil
}

// generateToken returns a hex-encoded 32-byte random session token.
func generateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// newRequestID returns a short random identifier for approvals.
func newRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "req"
	}
	return hex.EncodeToString(buf)
}

// LANAddresses returns the non-loopback IPv4 addresses of the local machine.
// Link-local (169.254/16) addresses are skipped because they are rarely
// reachable from another device.
func LANAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil {
			continue
		}
		ip4 := ip.To4()
		if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() || ip4.IsUnspecified() {
			continue
		}
		out = append(out, ip4.String())
	}
	sort.Strings(out)
	return out
}

// authManager holds the optional password and the in-memory session tokens.
// It is safe for concurrent use.
type authManager struct {
	mu       sync.Mutex
	password string
	enabled  bool
	tokens   map[string]time.Time
}

// newAuthManager builds the auth state. An empty password with enabled=true
// is rejected by callers; here it simply behaves as "no valid password".
func newAuthManager(password string, enabled bool) *authManager {
	password = strings.TrimSpace(password)
	return &authManager{
		password: password,
		enabled:  enabled,
		tokens:   make(map[string]time.Time),
	}
}

// Enabled reports whether authentication is required.
func (a *authManager) Enabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.enabled
}

// verify compares the supplied password in constant time. Hashing both sides
// first keeps the comparison independent of the input length.
func (a *authManager) verify(given string) bool {
	a.mu.Lock()
	enabled, want := a.enabled, a.password
	a.mu.Unlock()
	if !enabled {
		return true
	}
	if want == "" {
		return false
	}
	gotSum := sha256.Sum256([]byte(given))
	wantSum := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotSum[:], wantSum[:]) == 1
}

// issue creates and stores a fresh session token.
func (a *authManager) issue() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked()
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	a.tokens[token] = time.Now()
	return token, nil
}

// valid reports whether token is a live session.
func (a *authManager) valid(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	created, ok := a.tokens[token]
	if !ok {
		return false
	}
	if time.Since(created) > sessionTokenTTL {
		delete(a.tokens, token)
		return false
	}
	return true
}

// revoke removes a session token.
func (a *authManager) revoke(token string) {
	if token == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, token)
}

func (a *authManager) pruneLocked() {
	now := time.Now()
	for token, created := range a.tokens {
		if now.Sub(created) > sessionTokenTTL {
			delete(a.tokens, token)
		}
	}
}

// setSessionCookie stores the token in an HttpOnly, SameSite=Lax cookie.
func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTokenTTL.Seconds()),
	})
}

// clearSessionCookie expires the session cookie.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// sessionToken returns the token from the request cookie, if any.
func sessionToken(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// authorized reports whether the request may reach a protected /api route.
func (s *Server) authorized(r *http.Request) bool {
	if s.auth == nil || !s.auth.Enabled() {
		return true
	}
	return s.auth.valid(sessionToken(r))
}

// authMiddleware gates every /api route except login and auth status.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if !strings.HasPrefix(path, "/api/") || path == "/api/login" || path == "/api/auth/status" {
			next.ServeHTTP(w, r)
			return
		}
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type loginRequest struct {
	Password string `json:"password"`
}

// handleLogin verifies the password and issues a session cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.auth == nil || !s.auth.Enabled() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auth_enabled": false})
		return
	}
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !s.auth.verify(req.Password) {
		writeError(w, http.StatusUnauthorized, "invalid password")
		return
	}
	token, err := s.auth.issue()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auth_enabled": true})
}

// handleLogout revokes the session token and expires the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.auth != nil {
		s.auth.revoke(sessionToken(r))
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAuthStatus reports whether auth is on and whether this browser is
// already logged in. It is reachable without a session.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	enabled := s.auth != nil && s.auth.Enabled()
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_enabled":  enabled,
		"authenticated": s.authorized(r),
	})
}

// ---------------------------------------------------------------------------
// Small JSON helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// decodeJSON decodes a small JSON request body with a size limit.
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(v)
}
