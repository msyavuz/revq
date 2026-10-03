package web

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/msyavuz/revq/internal/store"
)

const sessionCookie = "revq_session"

// limiter slows password guessing: a handful of failures per address, then a wait.
type limiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

const (
	maxFails   = 8
	failWindow = 10 * time.Minute
)

func (l *limiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-failWindow)
	recent := l.fails[key][:0]
	for _, t := range l.fails[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	l.fails[key] = recent
	return len(recent) >= maxFails
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[key] = append(l.fails[key], time.Now())
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func setSession(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
}

// guard requires a session for everything but the login page and static
// files, forces the default password to be changed, and blocks cross-site POSTs.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "cross-site request blocked", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request blocked", http.StatusForbidden)
					return
				}
			}
		}
		path := r.URL.Path
		if path == "/login" || strings.HasPrefix(path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		if !s.st.SessionValid(sessionToken(r)) {
			if r.Header.Get("HX-Request") != "" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if path != "/account" && path != "/logout" {
			if u, err := s.st.User(); err == nil && u.MustChange {
				if r.Header.Get("HX-Request") != "" {
					w.Header().Set("HX-Redirect", "/account")
					return
				}
				http.Redirect(w, r, "/account", http.StatusSeeOther)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.st.SessionValid(sessionToken(r)) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderLogin(w, http.StatusOK, "", "")
}

func (s *Server) renderLogin(w http.ResponseWriter, status int, username, errMsg string) {
	u, _ := s.st.User()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	data := map[string]any{"Username": username, "Error": errMsg, "FirstRun": u.MustChange}
	if err := s.tpl["login"].ExecuteTemplate(w, "login", data); err != nil {
		s.log.Error("render", "tpl", "login", "err", err)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	username := strings.TrimSpace(r.FormValue("username"))
	if s.logins.blocked(ip) {
		s.renderLogin(w, http.StatusTooManyRequests, username, "Too many failed attempts. Wait ten minutes and try again.")
		return
	}
	u, err := s.st.User()
	if err != nil {
		s.fail(w, err)
		return
	}
	// Always run the password check so a wrong username costs the same time.
	okPass := u.CheckPassword(r.FormValue("password"))
	if !okPass || username != u.Username {
		s.logins.fail(ip)
		s.log.Warn("failed login", "ip", ip)
		s.renderLogin(w, http.StatusUnauthorized, username, "Wrong username or password.")
		return
	}
	s.logins.reset(ip)
	token, err := s.st.CreateSession()
	if err != nil {
		s.fail(w, err)
		return
	}
	setSession(w, r, token, 30*24*3600)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	_ = s.st.DeleteSession(sessionToken(r))
	setSession(w, r, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	u, err := s.st.User()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "account", "Account", map[string]any{"User": u, "MinLen": store.MinPasswordLen})
}

func (s *Server) saveAccount(w http.ResponseWriter, r *http.Request) {
	u, err := s.st.User()
	if err != nil {
		s.fail(w, err)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	next, confirm := r.FormValue("new_password"), r.FormValue("confirm_password")
	switch {
	case !u.CheckPassword(r.FormValue("current_password")):
		back(w, r, "/account", "Current password is wrong. Nothing was changed.")
		return
	case username == "":
		back(w, r, "/account", "Username can't be empty.")
		return
	case next == "" && u.MustChange:
		back(w, r, "/account", "Choose a new password to replace the default one.")
		return
	case next != "" && len(next) < store.MinPasswordLen:
		back(w, r, "/account", "New password is too short. Use at least 8 characters.")
		return
	case next != "" && next != confirm:
		back(w, r, "/account", "The two new passwords don't match.")
		return
	case next == store.DefaultPassword:
		back(w, r, "/account", "Pick something other than the default password.")
		return
	}
	u.Username = username
	if next != "" {
		u.SetPassword(next)
		u.MustChange = false
	}
	if err := s.st.SaveUser(u); err != nil {
		s.fail(w, err)
		return
	}
	msg := "Account saved"
	if next != "" {
		// A new password signs out every other browser.
		_ = s.st.DeleteOtherSessions(sessionToken(r))
		msg = "Password changed. Other browsers were signed out."
	}
	back(w, r, "/account", msg)
}
