package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"maya-moodlink/internal/moodle"
)

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "cuerpo JSON no válido")
		return false
	}
	return true
}

type tokenResponse struct {
	// TokenEnc es el token ya CIFRADO. Es opaco para quien lo recibe: Odoo lo guarda tal
	// cual en res_users.moodle_token_enc y no puede descifrarlo (no tiene la clave).
	TokenEnc       string `json:"token_enc"`
	MoodleUsername string `json:"moodle_username"`
	MoodleUserID   int    `json:"moodle_user_id"`
}

// POST /v1/users/{user}/token  {"username": "...", "password": "..."}
//
// Es la solicitud de token. Odoo ha detectado que el usuario no tiene token (o Moodle lo
// rechazó), le ha pedido la contraseña de Moodle y la envía aquí. El gateway la canjea en
// Moodle (login/token.php), comprueba el token y lo devuelve CIFRADO. No escribe nada en la
// base de datos: Odoo lo guarda con su ORM. Ni el token en claro ni la contraseña salen del
// gateway, y la contraseña no se guarda ni se registra.
func (s *Server) handleTokenRequest(w http.ResponseWriter, r *http.Request) {
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

	issued, err := s.Auth.IssueToken(r.Context(), user, body.Username, body.Password)
	if err != nil {
		if errors.Is(err, moodle.ErrInvalidLogin) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_credentials", "Moodle rechaza el usuario o la contraseña")
			return
		}
		s.writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		TokenEnc:       issued.TokenEnc,
		MoodleUsername: issued.MoodleUsername,
		MoodleUserID:   issued.MoodleUserID,
	})
}
