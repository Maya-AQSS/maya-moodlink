package moodle

import (
	"errors"
	"fmt"
)

var (
	// ErrOverloaded: no se obtuvo hueco de concurrencia hacia Moodle antes de agotar el contexto.
	ErrOverloaded = errors.New("moodle: demasiadas peticiones concurrentes")
	// ErrUnavailable: Moodle no responde o devolvió 429/502/503/504 tras agotar reintentos.
	ErrUnavailable = errors.New("moodle: servicio no disponible")
	// ErrInvalidToken: Moodle rechazó el token (inexistente, caducado, revocado).
	ErrInvalidToken = errors.New("moodle: token inválido o caducado")
	// ErrInvalidLogin: usuario/contraseña incorrectos en login/token.php.
	ErrInvalidLogin = errors.New("moodle: credenciales incorrectas")
)

// Error es una excepción devuelta por Moodle. Moodle responde HTTP 200 con
// {"exception": ..., "errorcode": ..., "message": ...}, así que hay que detectarla en el cuerpo.
type Error struct {
	Function string
	Code     string
	Message  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("moodle %s: %s (%s)", e.Function, e.Message, e.Code)
}

// Unwrap permite usar errors.Is(err, ErrInvalidToken) / ErrInvalidLogin.
func (e *Error) Unwrap() error {
	switch e.Code {
	case "invalidtoken":
		return ErrInvalidToken
	case "invalidlogin":
		return ErrInvalidLogin
	}
	return nil
}

// transientError marca fallos de red o de servidor que merece la pena reintentar.
type transientError struct{ err error }

func (e *transientError) Error() string        { return "moodle no disponible: " + e.err.Error() }
func (e *transientError) Unwrap() error        { return e.err }
func (e *transientError) Is(target error) bool { return target == ErrUnavailable }

func isTransient(err error) bool {
	var t *transientError
	return errors.As(err, &t)
}
