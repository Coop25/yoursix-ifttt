package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"yoursix-ifttt/internal/config"
)

const (
	sessionCookie = "y6ifttt_session"
	stateCookie   = "y6ifttt_oauth_state"
)

// Role is derived from config on every request, so removing an ID from the
// env and restarting revokes access even for unexpired sessions.
type Role string

const (
	RoleNone   Role = ""
	RoleViewer Role = "viewer"
	RoleAdmin  Role = "admin"
)

type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	AvatarURL   string `json:"avatarUrl,omitempty"`
	Role        Role   `json:"role"`
}

type sessionClaims struct {
	Sub         string `json:"sub"`
	Username    string `json:"u"`
	DisplayName string `json:"n"`
	Avatar      string `json:"a,omitempty"`
	Exp         int64  `json:"exp"`
}

type auth struct {
	cfg     config.AuthConfig
	devAuth bool
	secret  []byte
	client  *http.Client
}

func newAuth(cfg config.AuthConfig, devAuth bool) *auth {
	secret := []byte(cfg.SessionSecret)
	if len(secret) == 0 {
		// Dev mode without a configured secret: sessions die with the process.
		secret = make([]byte, 32)
		_, _ = rand.Read(secret)
	}
	return &auth{cfg: cfg, devAuth: devAuth, secret: secret, client: &http.Client{Timeout: 15 * time.Second}}
}

func (a *auth) roleFor(id string) Role {
	if _, ok := a.cfg.AdminUserIDs[id]; ok {
		return RoleAdmin
	}
	if _, ok := a.cfg.ViewerUserIDs[id]; ok {
		return RoleViewer
	}
	return RoleNone
}

func (a *auth) sign(v string) string {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *auth) seal(v string) string { return v + "." + a.sign(v) }

func (a *auth) open(sealed string) (string, bool) {
	i := strings.LastIndexByte(sealed, '.')
	if i < 0 {
		return "", false
	}
	v, sig := sealed[:i], sealed[i+1:]
	return v, hmac.Equal([]byte(sig), []byte(a.sign(v)))
}

func (a *auth) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(maxAge.Seconds()),
	})
}

func (a *auth) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: a.cfg.CookieSecure, MaxAge: -1})
}

// userFromRequest returns the signed-in user, or ok=false.
func (a *auth) userFromRequest(r *http.Request) (User, bool) {
	if a.devAuth {
		// Never honour dev auth for traffic that came through Cloudflare, in
		// case the flag is left on in a tunnelled deployment.
		if r.Header.Get("Cf-Ray") != "" || r.Header.Get("Cf-Connecting-Ip") != "" {
			return User{}, false
		}
		return User{ID: "dev", Username: "dev", DisplayName: "Local dev", Role: RoleAdmin}, true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return User{}, false
	}
	payload, ok := a.open(c.Value)
	if !ok {
		return User{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return User{}, false
	}
	var cl sessionClaims
	if json.Unmarshal(raw, &cl) != nil || time.Now().Unix() > cl.Exp {
		return User{}, false
	}
	role := a.roleFor(cl.Sub)
	if role == RoleNone {
		return User{}, false
	}
	return User{ID: cl.Sub, Username: cl.Username, DisplayName: cl.DisplayName, AvatarURL: cl.Avatar, Role: role}, true
}

func (a *auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	if a.devAuth {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	state := hex.EncodeToString(b)
	a.setCookie(w, stateCookie, a.seal(state), 10*time.Minute)
	q := url.Values{
		"client_id":     {a.cfg.ClientID},
		"response_type": {"code"},
		"redirect_uri":  {a.cfg.RedirectURL},
		"scope":         {"identify"},
		"state":         {state},
		"prompt":        {"none"},
	}
	http.Redirect(w, r, "https://discord.com/oauth2/authorize?"+q.Encode(), http.StatusFound)
}

func (a *auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := r.Cookie(stateCookie)
	a.clearCookie(w, stateCookie)
	if err != nil {
		http.Error(w, "login expired, try again", http.StatusBadRequest)
		return
	}
	state, ok := a.open(c.Value)
	if !ok || state == "" || !hmac.Equal([]byte(state), []byte(q.Get("state"))) {
		http.Error(w, "invalid login state", http.StatusBadRequest)
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		http.Redirect(w, r, "/?login=cancelled", http.StatusFound)
		return
	}

	du, err := a.exchange(r.Context(), q.Get("code"))
	if err != nil {
		http.Error(w, "discord login failed", http.StatusBadGateway)
		return
	}
	if a.roleFor(du.ID) == RoleNone {
		http.Redirect(w, r, "/?login=denied", http.StatusFound)
		return
	}

	name := du.GlobalName
	if name == "" {
		name = du.Username
	}
	avatar := ""
	if du.Avatar != "" {
		avatar = fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png?size=64", du.ID, du.Avatar)
	}
	claims, _ := json.Marshal(sessionClaims{
		Sub: du.ID, Username: du.Username, DisplayName: name, Avatar: avatar,
		Exp: time.Now().Add(a.cfg.SessionDuration).Unix(),
	})
	a.setCookie(w, sessionCookie, a.seal(base64.RawURLEncoding.EncodeToString(claims)), a.cfg.SessionDuration)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, sessionCookie)
	w.WriteHeader(http.StatusNoContent)
}

type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Avatar     string `json:"avatar"`
}

func (a *auth) exchange(ctx context.Context, code string) (discordUser, error) {
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {a.cfg.RedirectURL},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://discord.com/api/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return discordUser{}, err
	}
	req.SetBasicAuth(a.cfg.ClientID, a.cfg.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := a.doJSON(req, &tok); err != nil {
		return discordUser{}, fmt.Errorf("token exchange: %w", err)
	}
	if tok.AccessToken == "" {
		return discordUser{}, errors.New("token exchange: empty access token")
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://discord.com/api/users/@me", nil)
	if err != nil {
		return discordUser{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	var u discordUser
	if err := a.doJSON(req, &u); err != nil {
		return discordUser{}, fmt.Errorf("fetch user: %w", err)
	}
	if u.ID == "" {
		return discordUser{}, errors.New("fetch user: empty id")
	}
	return u, nil
}

func (a *auth) doJSON(req *http.Request, out any) error {
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}
