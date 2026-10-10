// Package server expone la API HTTP del gateway.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"maya-moodle-gateway/internal/moodle"
	"maya-moodle-gateway/internal/tokens"
)

// userHeader identifica de momento al usuario que hace la petición.
//
// ATENCIÓN: es un marcador provisional. Cualquiera que llegue al servicio puede poner
// esta cabecera y suplantar a otro usuario. Antes de exponer el servicio hay que
// sustituirlo por autenticación real (JWT / mTLS) y añadir la autorización por aula.
const userHeader = "X-Maya-User"

type Deps struct {
	Log            *slog.Logger
	Moodle         *moodle.Client
	Tokens         *tokens.Manager
	MoodleService  string
	AdminAPIKey    string
	RequestTimeout time.Duration
}

type Server struct {
	Deps
	ready struct {
		mu        sync.Mutex
		checkedAt time.Time
		ok        bool
	}
}

func New(d Deps) http.Handler {
	if d.RequestTimeout <= 0 {
		d.RequestTimeout = 30 * time.Second
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	s := &Server{Deps: d}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	//mux.HandleFunc("GET /v1/courses/{courseID}/students", s.withUser(s.handleStudents))

	mux.HandleFunc("GET /v1/admin/users/{user}/token", s.withAdmin(s.handleTokenStatus))
	mux.HandleFunc("PUT /v1/admin/users/{user}/token", s.withAdmin(s.handleTokenPut))
	mux.HandleFunc("POST /v1/admin/users/{user}/token/login", s.withAdmin(s.handleTokenLogin))
	mux.HandleFunc("DELETE /v1/admin/users/{user}/token", s.withAdmin(s.handleTokenDelete))

	return s.logging(s.recoverer(mux))
}

// ---------------------------------------------------------------- health

// handleHealthz: liveness. Solo indica que el proceso responde; no depende de Moodle.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz: readiness. Comprueba que Moodle es alcanzable (resultado cacheado 10 s
// para que un sondeo frecuente no genere tráfico hacia Moodle).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if !s.moodleReachable(r.Context()) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "moodle_unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) moodleReachable(ctx context.Context) bool {
	s.ready.mu.Lock()
	defer s.ready.mu.Unlock()
	if !s.ready.checkedAt.IsZero() && time.Since(s.ready.checkedAt) < 10*time.Second {
		return s.ready.ok
	}
	err := s.Moodle.Ping(ctx)
	s.ready.checkedAt = time.Now()
	s.ready.ok = err == nil
	if err != nil {
		s.Log.Warn("moodle no alcanzable", "error", err)
	}
	return s.ready.ok
}

// ---------------------------------------------------------------- middlewares

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// logging asigna un id a la petición, impone el timeout global y registra una línea por
// petición. No registra query string, cuerpos ni cabeceras de autorización.
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := newRequestID()
		w.Header().Set("X-Request-ID", id)

		ctx, cancel := context.WithTimeout(r.Context(), s.RequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			s.Log.Info("request",
				"id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"user", r.Header.Get(userHeader),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				s.Log.Error("panic en handler", "panic", p, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal", "error interno")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withUser exige la cabecera de identidad (provisional, ver userHeader).
func (s *Server) withUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validUser(r.Header.Get(userHeader)) {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "falta o no es válida la cabecera "+userHeader)
			return
		}
		next(w, r)
	}
}

// withAdmin protege los endpoints /v1/admin/* con un Bearer estático (comparación en
// tiempo constante) y valida el {user} de la ruta.
func (s *Server) withAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.AdminAPIKey == "" {
			writeError(w, http.StatusServiceUnavailable, "admin_disabled", "ADMIN_API_KEY no está configurada")
			return
		}
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "se requiere Authorization: Bearer")
			return
		}
		got := sha256.Sum256([]byte(h[len(prefix):]))
		want := sha256.Sum256([]byte(s.AdminAPIKey))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "credencial de administración incorrecta")
			return
		}
		if !validUser(r.PathValue("user")) {
			writeError(w, http.StatusBadRequest, "invalid_user", "identificador de usuario no válido")
			return
		}
		next(w, r)
	}
}

// validUser limita los identificadores a un alfabeto seguro (se usan como clave y en logs).
func validUser(u string) bool {
	if u == "" || len(u) > 128 {
		return false
	}
	for _, c := range u {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-', c == '@':
		default:
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- respuestas

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var b errorBody
	b.Error.Code = code
	b.Error.Message = msg
	writeJSON(w, status, b)
}

// writeUpstreamError traduce los errores del Manager de tokens y del cliente Moodle a
// respuestas HTTP coherentes.
func (s *Server) writeUpstreamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tokens.ErrNoToken):
		writeError(w, http.StatusUnauthorized, "moodle_token_missing", "el usuario no tiene token de Moodle registrado")
	case errors.Is(err, tokens.ErrTokenInvalid), errors.Is(err, moodle.ErrInvalidToken):
		writeError(w, http.StatusUnauthorized, "moodle_token_invalid", "el token de Moodle del usuario no es válido o ha caducado; debe renovarse")
	case errors.Is(err, moodle.ErrOverloaded):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "overloaded", "demasiadas peticiones hacia Moodle; reintente en unos instantes")
	case errors.Is(err, context.Canceled):
		writeError(w, 499, "client_closed_request", "el cliente cerró la conexión")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "upstream_timeout", "Moodle no respondió a tiempo")
	case errors.Is(err, moodle.ErrUnavailable):
		writeError(w, http.StatusBadGateway, "moodle_unavailable", "Moodle no está disponible")
	default:
		var me *moodle.Error
		if errors.As(err, &me) {
			switch me.Code {
			case "invalidrecord", "invalidcourseid":
				writeError(w, http.StatusNotFound, "not_found", me.Message)
			case "nopermissions", "accessexception", "requireloginerror":
				writeError(w, http.StatusForbidden, "moodle_forbidden", me.Message)
			default:
				writeError(w, http.StatusBadGateway, "moodle_error", me.Message+" ("+me.Code+")")
			}
			return
		}
		s.Log.Error("error no clasificado", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "error interno")
	}
}
