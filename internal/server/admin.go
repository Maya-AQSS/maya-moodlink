package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"maya-moodlink/internal/moodle"
	"maya-moodlink/internal/tokens"
)

// tokenStatusDTO es una ESTRUCTURA (struct) que define el formato JSON que devolveremos al cliente.
// DTO significa "Data Transfer Object".

// Struct Tags (Etiquetas entre comillas invertidas “):
// Le dicen al paquete `encoding/json` cómo debe llamarse la clave en el JSON resultante.
// Ejemplo: `json:"has_token"` convierte la propiedad Go "HasToken" a "has_token" en la respuesta JSON.
// `,omitempty` le dice a Go: "si este campo está vacío o es 'nil', NO lo incluyas en el JSON".
type tokenStatusDTO struct {
	User           string     `json:"user"`
	HasToken       bool       `json:"has_token"`
	Valid          bool       `json:"valid"`
	MoodleUserID   int        `json:"moodle_user_id,omitempty"`
	MoodleUsername string     `json:"moodle_username,omitempty"`
	ValidUntil     *time.Time `json:"valid_until,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
}

// Filtro de seguridad y limpieza para procesar las peticiones JSON que envían los usuarios.
// Parámetros:
// - w: Dónde escribiremos la respuesta si hay error.
// - r: La petición HTTP entrante.
// - dst: 'any' (interface{} en versiones antiguas) significa que acepta cualquier tipo de dato. Es la variable destino donde volcaremos el JSON.
// Devuelve: bool (true si el JSON era válido, false si falló).
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	// Protegemos el servidor contra ataques de denegación de servicio (DoS) limitando el cuerpo
	// de la petición a un máximo de 1 Megabyte (1 << 20 bytes usando desplazamiento de bits).
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// Creamos un decodificador JSON leyendo desde el cuerpo de la petición.
	dec := json.NewDecoder(r.Body)

	// DisallowUnknownFields: hace que la decodificación falle si el cliente envía campos JSON
	// que no existen en nuestro struct de destino (evita datos basura o no deseados).
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "cuerpo JSON no válido")
		return false
	}
	return true
}

// Unifica cómo se responde la información de un token al cliente.
// En Go no hay clases, pero puedes asociar funciones a un struct anteponiendo `(s *Server)`.
// Esto convierte a `writeTokenStatus` en un metodo de la estructura `Server`.
// `s` actúa como el equivalente a `this` o `self` en otros lenguajes (un puntero a la instancia activa).
func (s *Server) writeTokenStatus(w http.ResponseWriter, r *http.Request, user string) {
	st, ok, err := s.Tokens.Status(r.Context(), user)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, tokenStatusDTO{User: user})
		return
	}
	updated := st.UpdatedAt
	writeJSON(w, http.StatusOK, tokenStatusDTO{
		User:           user,
		HasToken:       true,
		Valid:          st.Valid,
		MoodleUserID:   st.MoodleUserID,
		MoodleUsername: st.MoodleUsername,
		ValidUntil:     st.ValidUntil,
		UpdatedAt:      &updated,
	})
}

// GET /v1/admin/users/{user}/token : estado del token (nunca devuelve el token).
func (s *Server) handleTokenStatus(w http.ResponseWriter, r *http.Request) {
	s.writeTokenStatus(w, r, r.PathValue("user"))
}

// PUT /v1/admin/users/{user}/token  {"token": "...", "valid_until": "2026-12-31T00:00:00Z"}
// Registra un token ya existente (p. ej. creado por el admin de Moodle). Antes de guardarlo
// se valida contra Moodle, y de paso se averigua a qué usuario de Moodle pertenece.
func (s *Server) handleTokenPut(w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user")
	var body struct {
		Token      string     `json:"token"`
		ValidUntil *time.Time `json:"valid_until"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Token == "" {
		writeError(w, http.StatusBadRequest, "invalid_token", "el campo token es obligatorio")
		return
	}

	info, err := s.Moodle.SiteInfo(r.Context(), body.Token)
	if err != nil {
		if errors.Is(err, moodle.ErrInvalidToken) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_token", "Moodle rechaza ese token")
			return
		}
		s.writeUpstreamError(w, err)
		return
	}

	err = s.Tokens.Save(r.Context(), user, tokens.Record{
		Token:          body.Token,
		MoodleUserID:   info.UserID,
		MoodleUsername: info.Username,
		ValidUntil:     body.ValidUntil,
	})
	if err != nil {
		s.Log.Error("no se pudo guardar el token", "user", user, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "no se pudo guardar el token")
		return
	}
	s.writeTokenStatus(w, r, user)
}

// POST /v1/admin/users/{user}/token/login  {"username": "...", "password": "..."}
// Obtiene un token nuevo de Moodle (login/token.php) con las credenciales del usuario.
// La contraseña no se guarda ni se registra: solo se guarda el token resultante. Sirve
// tanto para la primera obtención como para renovar un token caducado o revocado.
func (s *Server) handleTokenLogin(w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user")
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Username == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "invalid_credentials", "username y password son obligatorios")
		return
	}

	token, err := s.Moodle.RequestToken(r.Context(), body.Username, body.Password, s.MoodleService)
	if err != nil {
		if errors.Is(err, moodle.ErrInvalidLogin) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_credentials", "Moodle rechaza el usuario o la contraseña")
			return
		}
		s.writeUpstreamError(w, err)
		return
	}

	info, err := s.Moodle.SiteInfo(r.Context(), token)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	err = s.Tokens.Save(r.Context(), user, tokens.Record{
		Token:          token,
		MoodleUserID:   info.UserID,
		MoodleUsername: info.Username,
	})
	if err != nil {
		s.Log.Error("no se pudo guardar el token", "user", user, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "no se pudo guardar el token")
		return
	}
	s.writeTokenStatus(w, r, user)
}

// DELETE /v1/admin/users/{user}/token
func (s *Server) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.Tokens.Delete(r.Context(), r.PathValue("user")); err != nil {
		s.Log.Error("no se pudo borrar el token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "no se pudo borrar el token")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
