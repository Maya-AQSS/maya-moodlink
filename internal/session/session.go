// Package session decide con qué token de Moodle se hace cada petición y, si hace falta,
// lo obtiene o lo renueva.
//
//   - Profesores: el token vive cifrado en res_users (lo escribe Odoo con su ORM). Si Moodle
//     lo rechaza, no se renueva aquí: se devuelve ErrInvalidToken y Odoo pide la contraseña.
//   - Cuentas de servicio (administradores): su contraseña la da un CredentialProvider. El
//     token se mantiene solo en memoria y, si Moodle lo rechaza, se renueva y se reintenta
//     la operación UNA vez. No se persiste porque siempre se puede volver a generar.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"maya-moodlink/internal/moodle"
	"maya-moodlink/internal/tokens"
)

var (
	ErrNoToken        = errors.New("session: el usuario no tiene token de Moodle")
	ErrUserInactive   = errors.New("session: el usuario no existe o está archivado")
	ErrServiceAccount = errors.New("session: fallo con la cuenta de servicio de Moodle")
)

type Options struct {
	Store   tokens.Store
	Cipher  *tokens.Cipher
	Creds   CredentialProvider // nil = no hay cuentas de servicio
	Moodle  *moodle.Client
	Service string // shortname del servicio web para login/token.php
	Log     *slog.Logger
}

type Authenticator struct {
	store   tokens.Store
	cipher  *tokens.Cipher
	creds   CredentialProvider
	moodle  *moodle.Client
	service string
	log     *slog.Logger

	cacheMu sync.RWMutex
	cache   map[string]string // tokens de cuentas de servicio, por id de usuario
	flight  *flightGroup
}

func New(o Options) *Authenticator {
	if o.Creds == nil {
		o.Creds = noCredentials{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Authenticator{
		store:   o.Store,
		cipher:  o.Cipher,
		creds:   o.Creds,
		moodle:  o.Moodle,
		service: o.Service,
		log:     o.Log,
		cache:   map[string]string{},
		flight:  &flightGroup{m: map[string]*flightCall{}},
	}
}

// Ejecuta fn con el token de Moodle del usuario (el id de usuario de Odoo).
//
// Errores que devuelve Do, además de los de fn:
//   - ErrUserInactive: el usuario no existe o está archivado.
//   - ErrNoToken: profesor que aún no ha introducido su contraseña.
//   - moodle.ErrInvalidToken: el token de un profesor ya no vale (Odoo debe pedir la contraseña).
//   - ErrServiceAccount: no se pudo autenticar una cuenta de servicio (problema de configuración).
func (a *Authenticator) Do(ctx context.Context, user string, fn func(token string) error) error {
	password, isService, err := a.creds.Password(ctx, user)
	if err != nil {
		return err
	}
	row, ok, err := a.store.Get(ctx, user)
	if err != nil {
		return err
	}
	if !ok || !row.Active {
		return ErrUserInactive
	}
	if isService {
		return a.doService(ctx, user, row, password, fn)
	}
	return a.doUser(user, row, fn)
}

func (a *Authenticator) doUser(user string, row tokens.Row, fn func(string) error) error {
	if row.TokenEnc == "" {
		return ErrNoToken
	}
	token, err := a.cipher.Decrypt(user, row.TokenEnc)
	if err != nil {
		return err
	}
	return fn(token)
}

func (a *Authenticator) doService(ctx context.Context, user string, row tokens.Row, password string, fn func(string) error) error {
	if row.MoodleUsername == "" {
		return fmt.Errorf("%w: el usuario %s no tiene moodle_username configurado", ErrServiceAccount, user)
	}

	token := a.cached(user)
	if token == "" {
		var err error
		token, err = a.renew(ctx, user, row.MoodleUsername, password, "")
		if err != nil {
			return err
		}
	}

	err := fn(token)
	if !errors.Is(err, moodle.ErrInvalidToken) {
		return err
	}

	// Moodle rechazó el token: se renueva y se reintenta una vez. Reintentar es seguro
	// aunque la operación no sea idempotente: Moodle valida el token antes de ejecutar nada.
	a.log.Info("token de cuenta de servicio rechazado por Moodle: renovando", "user", user)
	token, err = a.renew(ctx, user, row.MoodleUsername, password, token)
	if err != nil {
		return err
	}
	err = fn(token)
	if errors.Is(err, moodle.ErrInvalidToken) {
		return fmt.Errorf("%w: Moodle rechaza el token recién obtenido", ErrServiceAccount)
	}
	return err
}

// renew obtiene un token nuevo para la cuenta de servicio. Si varias peticiones fallan a la
// vez con el mismo token, solo una va a Moodle; las demás reutilizan el resultado.
// failed es el token que acaba de fallar ("" si aún no había ninguno).
func (a *Authenticator) renew(ctx context.Context, user, username, password, failed string) (string, error) {
	// La clave incluye el token fallido para que una renovación "inicial" en curso no se
	// confunda con la renovación posterior de un token que acaba de ser rechazado.
	return a.flight.do(user+"\x00"+failed, func() (string, error) {
		if cur := a.cached(user); cur != "" && cur != failed {
			return cur, nil // otra petición ya lo renovó
		}
		// Desacoplado de la petición que lo inició: si ésta se cancela, las demás que
		// esperan el resultado no deben fallar por eso.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		token, err := a.moodle.RequestToken(rctx, username, password, a.service)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrServiceAccount, err)
		}
		a.setCached(user, token)
		return token, nil
	})
}

func (a *Authenticator) cached(user string) string {
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	return a.cache[user]
}

func (a *Authenticator) setCached(user, token string) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	a.cache[user] = token
}

// Issued es el resultado de solicitar un token a Moodle con usuario y contraseña.
type Issued struct {
	TokenEnc       string // token cifrado, listo para que Odoo lo guarde tal cual
	MoodleUsername string
	MoodleUserID   int
}

// IssueToken canjea usuario y contraseña de Moodle por un token, comprueba que Moodle lo
// acepta y lo devuelve CIFRADO. No escribe nada en la base de datos: lo guarda Odoo.
// La contraseña no se guarda ni se registra.
func (a *Authenticator) IssueToken(ctx context.Context, user, username, password string) (Issued, error) {
	token, err := a.moodle.RequestToken(ctx, username, password, a.service)
	if err != nil {
		return Issued{}, err
	}
	info, err := a.moodle.SiteInfo(ctx, token)
	if err != nil {
		return Issued{}, err
	}
	enc, err := a.cipher.Encrypt(user, token)
	if err != nil {
		return Issued{}, err
	}
	return Issued{TokenEnc: enc, MoodleUsername: info.Username, MoodleUserID: info.UserID}, nil
}

// ---------------------------------------------------------------- deduplicación

type flightCall struct {
	wg  sync.WaitGroup
	val string
	err error
}

// flightGroup evita ejecutar a la vez varias veces la misma operación (singleflight mínimo).
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

func (g *flightGroup) do(key string, fn func() (string, error)) (string, error) {
	g.mu.Lock()
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &flightCall{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	defer func() {
		c.wg.Done()
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
	}()
	c.val, c.err = fn()
	return c.val, c.err
}
