package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/clearsign"
)

type AuthManager struct {
	mu        sync.RWMutex
	sessions  map[string]time.Time
	publicKey openpgp.EntityList
}

func loadPublicKey() (string, error) {
	if val := os.Getenv("PGP_PUBLIC_KEY"); val != "" {
		return strings.ReplaceAll(val, `\n`, "\n"), nil
	}
	if path := os.Getenv("PGP_PUBLIC_KEY_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading PGP_PUBLIC_KEY_FILE: %w", err)
		}
		return string(data), nil
	}
	return "", nil
}

func NewAuthManager(armored string) (*AuthManager, error) {
	el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil {
		return nil, fmt.Errorf("parsing public key: %w", err)
	}
	a := &AuthManager{
		sessions:  make(map[string]time.Time),
		publicKey: el,
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			a.expireOldSessions()
		}
	}()
	return a, nil
}

func (a *AuthManager) NewChallenge() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("logger-auth:%d:%s", time.Now().Unix(), hex.EncodeToString(b)), nil
}

func (a *AuthManager) VerifySignature(armoredSig string) (bool, error) {
	block, _ := clearsign.Decode([]byte(armoredSig))
	if block == nil {
		return false, nil
	}

	challenge := strings.TrimSpace(string(block.Plaintext))
	parts := strings.SplitN(challenge, ":", 3)
	if len(parts) != 3 || parts[0] != "logger-auth" {
		return false, nil
	}
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false, nil
	}
	if time.Since(time.Unix(ts, 0)) > 5*time.Minute {
		return false, nil
	}

	_, err = openpgp.CheckDetachedSignature(a.publicKey, bytes.NewReader(block.Bytes), block.ArmoredSignature.Body)
	if err != nil {
		return false, nil
	}
	return true, nil
}

func (a *AuthManager) CreateSession() string {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	a.mu.Lock()
	a.sessions[token] = time.Now().Add(24 * time.Hour)
	a.mu.Unlock()
	return token
}

func (a *AuthManager) ValidateToken(token string) bool {
	if token == "" {
		return false
	}
	a.mu.RLock()
	exp, ok := a.sessions[token]
	a.mu.RUnlock()
	return ok && time.Now().Before(exp)
}

func (a *AuthManager) expireOldSessions() {
	now := time.Now()
	a.mu.Lock()
	for tok, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, tok)
		}
	}
	a.mu.Unlock()
}

func (a *AuthManager) HandleChallenge(w http.ResponseWriter, r *http.Request) {
	challenge, err := a.NewChallenge()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"challenge": challenge})
}

func (a *AuthManager) HandleVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Signature == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "signature required"})
		return
	}

	ok, err := a.VerifySignature(req.Signature)
	if err != nil || !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid signature"})
		return
	}

	token := a.CreateSession()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": token})
}

func (a *AuthManager) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimPrefix(h, "Bearer ")
		}
		if token == "" {
			token = r.URL.Query().Get("token")
		}

		if a.ValidateToken(token) {
			next.ServeHTTP(w, r)
			return
		}

		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
	})
}
