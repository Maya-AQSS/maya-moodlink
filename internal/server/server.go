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
	"strconv"
	"sync"
	"time"

	"maya-moodlink/internal/moodle"
	"maya-moodlink/internal/session"
	"maya-moodlink/internal/tokens"
)

// userHeader lleva el id del usuario de Odoo (res.users.id) en nombre del cual se opera.
//
// ATENCIÓN: la autenticación actual es una clave de servicio compartida (API_KEY). Quien la
// tenga puede actuar como cualquier usuario. Es válido mientras el único llamante sea Odoo,
// en red de confianza; la autorización por operación y aula irá a continuación.
const userHeader = "X-Maya-User"

type Deps struct {
	Log            *slog.Logger
	Moodle         *moodle.Client
	Auth           *session.Authenticator
	APIKey         string
	DBPing         func(ctx context.Context) error // nil si no hay base de datos (tests)
	RequestTimeout time.Duration
}

type Server struct {
	Deps
	ready struct {
		mu        sync.Mutex
		checkedAt time.Time
		failing   []string
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

	// mux.HandleFunc("GET /v1/courses/{courseID}/students", s.auth(s.withUser(s.handleStudents)))
	mux.HandleFunc("POST /v1/users/{user}/token", s.auth(s.withPathUser(s.handleTokenRequest)))
	mux.HandleFunc("GET /v1/assignments/{assignmentID}/submissions", s.auth(s.withUser(s.handleSubmissions)))

	return s.logging(s.recoverer(mux))
}

// ---------------------------------------------------------------- health

// handleHealthz: liveness. Solo indica que el proceso responde; no depende de nada externo.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz: readiness. Comprueba que Moodle y la base de datos de Odoo responden.
// El resultado se cachea 10 s para que un sondeo frecuente no genere tráfico.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if failing := s.failingDeps(r.Context()); len(failing) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "failing": failing})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) failingDeps(ctx context.Context) []string {
	s.ready.mu.Lock()
	defer s.ready.mu.Unlock()
	if !s.ready.checkedAt.IsZero() && time.Since(s.ready.checkedAt) < 10*time.Second {
		return s.ready.failing
	}

	var failing []string
	if err := s.Moodle.Ping(ctx); err != nil {
		failing = append(failing, "moodle")
		s.Log.Warn("moodle no alcanzable", "error", err)
	}
	if s.DBPing != nil {
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := s.DBPing(dctx); err != nil {
			failing = append(failing, "database")
			s.Log.Warn("base de datos de odoo no alcanzable", "error", err)
		}
	}
	s.ready.checkedAt = time.Now()
	s.ready.failing = failing
	return failing
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
// petición. No registra query string, cuerpos ni cabeceras de autorización: por el
// endpoint de token pasan contraseñas.
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

// auth exige la clave de servicio (Authorization: Bearer API_KEY), comparada en tiempo
// constante. Sin clave configurada falla cerrado.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.APIKey == "" {
			writeError(w, http.StatusServiceUnavailable, "api_key_not_configured", "API_KEY no está configurada")
			return
		}
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "se requiere Authorization: Bearer")
			return
		}
		got := sha256.Sum256([]byte(h[len(prefix):]))
		want := sha256.Sum256([]byte(s.APIKey))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "credencial incorrecta")
			return
		}
		next(w, r)
	}
}

// withUser exige la cabecera X-Maya-User con el id del usuario de Odoo.
func (s *Server) withUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validUser(r.Header.Get(userHeader)) {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "falta o no es válida la cabecera "+userHeader)
			return
		}
		next(w, r)
	}
}

// withPathUser valida el {user} de la ruta.
func (s *Server) withPathUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validUser(r.PathValue("user")) {
			writeError(w, http.StatusBadRequest, "invalid_user", "el usuario debe ser el id numérico de Odoo")
			return
		}
		next(w, r)
	}
}

// validUser: el usuario es el id de res.users de Odoo, en forma canónica ("7", no "007").
func validUser(u string) bool {
	id, err := strconv.ParseInt(u, 10, 64)
	return err == nil && id > 0 && strconv.FormatInt(id, 10) == u
}

// ---------------------------------------------------------------- respuestas

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		MoodleCode string `json:"moodle_code,omitempty"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var b errorBody
	b.Error.Code = code
	b.Error.Message = msg
	writeJSON(w, status, b)
}

func writeMoodleError(w http.ResponseWriter, status int, code string, me *moodle.Error) {
	var b errorBody
	b.Error.Code = code
	b.Error.Message = me.Message
	b.Error.MoodleCode = me.Code
	writeJSON(w, status, b)
}

// writeUpstreamError traduce los errores de la sesión y del cliente Moodle a HTTP.
//
// La distinción importante para quien llama (Odoo):
//
//	401 moodle_token_missing / moodle_token_invalid  -> el usuario debe (re)introducir su contraseña
//	403 moodle_forbidden                              -> Moodle le niega el permiso: reautenticarse NO lo arregla
//	403 user_inactive                                 -> el usuario no existe o está archivado
func (s *Server) writeUpstreamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrNoToken):
		writeError(w, http.StatusUnauthorized, "moodle_token_missing", "el usuario aún no se ha identificado en Moodle")
	case errors.Is(err, moodle.ErrInvalidToken):
		writeError(w, http.StatusUnauthorized, "moodle_token_invalid", "el token de Moodle del usuario no es válido; debe volver a identificarse")
	case errors.Is(err, tokens.ErrDecrypt):
		// No debería pasar salvo clave retirada o fila manipulada. Para el usuario es
		// recuperable (al identificarse de nuevo se sobrescribe), pero operaciones debe verlo.
		s.Log.Error("no se pudo descifrar un token guardado", "error", err)
		writeError(w, http.StatusUnauthorized, "moodle_token_invalid", "el token guardado no es utilizable; debe volver a identificarse")
	case errors.Is(err, session.ErrUserInactive):
		writeError(w, http.StatusForbidden, "user_inactive", "el usuario no existe o está archivado")
	case errors.Is(err, moodle.ErrOverloaded):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "overloaded", "demasiadas peticiones hacia Moodle; reintente en unos instantes")
	case errors.Is(err, context.Canceled):
		writeError(w, 499, "client_closed_request", "el cliente cerró la conexión")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "upstream_timeout", "Moodle o la base de datos no respondieron a tiempo")
	case errors.Is(err, moodle.ErrUnavailable):
		writeError(w, http.StatusBadGateway, "moodle_unavailable", "Moodle no está disponible")
	case errors.Is(err, session.ErrServiceAccount):
		// Problema de configuración (contraseña caducada, usuario mal puesto...): no es culpa
		// del usuario final ni se arregla pidiéndole nada.
		s.Log.Error("fallo con una cuenta de servicio de Moodle", "error", err)
		writeError(w, http.StatusBadGateway, "service_account_failed", "no se pudo autenticar la cuenta de servicio en Moodle")
	default:
		var me *moodle.Error
		if errors.As(err, &me) {
			switch me.Code {
			case "invalidrecord", "invalidcourseid":
				writeMoodleError(w, http.StatusNotFound, "not_found", me)
			case "nopermissions", "accessexception", "requireloginerror":
				writeMoodleError(w, http.StatusForbidden, "moodle_forbidden", me)
			default:
				writeMoodleError(w, http.StatusBadGateway, "moodle_error", me)
			}
			return
		}
		s.Log.Error("error no clasificado", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "error interno")
	}
}
