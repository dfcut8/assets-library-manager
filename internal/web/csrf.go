package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
)

const csrfCookieName = "alm_csrf"

type csrfContextKey struct{}

type csrfProtection struct {
	secret []byte
}

func newCSRF(secret []byte) csrfProtection {
	return csrfProtection{secret: append([]byte(nil), secret...)}
}

func (protection csrfProtection) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := protection.ensureToken(w, r)
		if err != nil {
			http.Error(w, "Unable to establish request protection.", http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, postBodyLimit(r.URL.Path))
			if !sameOrigin(r) || !protection.validRequestToken(r, token) {
				http.Error(w, "Request protection validation failed.", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfContextKey{}, token)))
	})
}

func postBodyLimit(path string) int64 {
	if strings.HasSuffix(path, "/reveal") {
		return revealBodyLimit
	}

	return metadataBodyLimit
}

func (protection csrfProtection) ensureToken(w http.ResponseWriter, r *http.Request) (string, error) {
	cookie, err := r.Cookie(csrfCookieName)
	if err == nil {
		if token, ok := protection.validCookie(cookie.Value); ok {
			return token, nil
		}
	}
	if err != nil && !errors.Is(err, http.ErrNoCookie) {
		return "", err
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(random)
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookieName, Value: protection.sign(token), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	return token, nil
}

func (protection csrfProtection) validRequestToken(r *http.Request, token string) bool {
	if err := r.ParseForm(); err != nil {
		return false
	}
	provided := r.Form.Get("csrf_token")
	if provided == "" {
		provided = r.Header.Get("X-CSRF-Token")
	}

	return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func (protection csrfProtection) validCookie(value string) (string, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	want := protection.sign(parts[0])
	if subtle.ConstantTimeCompare([]byte(value), []byte(want)) != 1 {
		return "", false
	}

	return parts[0], true
}

func (protection csrfProtection) sign(token string) string {
	mac := hmac.New(sha256.New, protection.secret)
	_, _ = mac.Write([]byte(token))

	return token + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}

	return origin == "http://"+r.Host || origin == "https://"+r.Host
}

func csrfToken(ctx context.Context) string {
	token, _ := ctx.Value(csrfContextKey{}).(string)

	return token
}
