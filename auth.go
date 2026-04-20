package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	adminUsername     = "admin_solanis"
	adminPasswordHash = "$2a$10$bjD70cVBMpKoVzMFtEFUp.9t9Qc8CEbP.kVo5fqb4slgBJA4znYPq"
	sessionCookieName = "session"
	sessionDuration   = 24 * time.Hour
)

type AuthManager struct {
	mu       sync.RWMutex
	sessions map[string]time.Time
}

func NewAuthManager() *AuthManager {
	a := &AuthManager{sessions: make(map[string]time.Time)}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			a.sweep()
		}
	}()
	return a
}

func (a *AuthManager) createSession() string {
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	a.mu.Lock()
	a.sessions[tok] = time.Now().Add(sessionDuration)
	a.mu.Unlock()
	return tok
}

func (a *AuthManager) IsAuthenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	a.mu.RLock()
	exp, ok := a.sessions[c.Value]
	a.mu.RUnlock()
	return ok && time.Now().Before(exp)
}

func (a *AuthManager) deleteSession(tok string) {
	a.mu.Lock()
	delete(a.sessions, tok)
	a.mu.Unlock()
}

func (a *AuthManager) sweep() {
	now := time.Now()
	a.mu.Lock()
	for tok, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, tok)
		}
	}
	a.mu.Unlock()
}

func (a *AuthManager) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.IsAuthenticated(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Accept") == "text/event-stream" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})
}

func (a *AuthManager) HandleLoginPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/login.html")
}

func (a *AuthManager) HandleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	user := r.FormValue("username")
	pass := r.FormValue("password")
	userOK := user == adminUsername
	passOK := bcrypt.CompareHashAndPassword([]byte(adminPasswordHash), []byte(pass)) == nil
	if !userOK || !passOK {
		http.Redirect(w, r, "/login?error=1", http.StatusFound)
		return
	}
	tok := a.createSession()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionDuration.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *AuthManager) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		a.deleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}
