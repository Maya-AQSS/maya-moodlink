package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// CredentialProvider entrega la contraseña de Moodle de las CUENTAS DE SERVICIO
// (los administradores). Para el resto de usuarios (los profesores) devuelve ok=false:
// su contraseña no se guarda en ninguna parte y se pide al usuario cuando hace falta.
//
// NOTA: Por ahora la implementación lee un fichero (StaticCredentials). En un futuro se implementará sobre Vault
type CredentialProvider interface {
	Password(ctx context.Context, user string) (password string, ok bool, err error)
}

// StaticCredentials: id de usuario de Odoo -> contraseña de Moodle.
type StaticCredentials map[string]string

func (c StaticCredentials) Password(_ context.Context, user string) (string, bool, error) {
	p, ok := c[user]
	return p, ok, nil
}

type noCredentials struct{}

func (noCredentials) Password(context.Context, string) (string, bool, error) { return "", false, nil }

// LoadCredentials lee un fichero JSON {"<id de usuario de Odoo>": {"password": "..."}}.
// Pensado para un secret montado (Docker/Kubernetes), no para el repositorio.
// El nombre de usuario de Moodle NO va aquí: se lee de res_users.moodle_username.
func LoadCredentials(path string) (StaticCredentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("leyendo credenciales de cuentas de servicio: %w", err)
	}
	var file map[string]struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("fichero de credenciales no válido: %w", err)
	}
	out := make(StaticCredentials, len(file))
	for user, c := range file {
		id, err := strconv.ParseInt(user, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != user {
			return nil, fmt.Errorf("fichero de credenciales: %q no es un id de usuario de Odoo", user)
		}
		if c.Password == "" {
			return nil, fmt.Errorf("fichero de credenciales: el usuario %s no tiene password", user)
		}
		out[user] = c.Password
	}
	return out, nil
}
