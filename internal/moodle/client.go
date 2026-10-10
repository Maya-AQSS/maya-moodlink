// Package moodle es el cliente del servidor de Moodle (web services REST).
//
// Decisiones de diseño:
//   - Un único http.Client por proceso con http.Transport afinado: las conexiones TCP/TLS
//     se reutilizan (keep-alive) en lugar de abrirse en cada petición.
//   - Un semáforo limita las peticiones simultáneas hacia Moodle. Protege al PHP de Moodle
//     de una avalancha y acota el uso de conexiones.
//   - Todo viaja por POST en el cuerpo (nunca en la URL): el token no acaba en logs de
//     proxies y no hay límite de longitud de URL.
//   - Solo se reintenta si la operación se declara idempotente.
package moodle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxResponseBytes = 64 << 20 // 64 MiB

type Options struct {
	BaseURL        string
	MaxConcurrency int           // peticiones simultáneas máximas hacia Moodle
	AttemptTimeout time.Duration // timeout de cada intento
	MaxRetries     int           // reintentos extra (solo idempotentes)
	Logger         *slog.Logger
}

type Client struct {
	baseURL        string
	wsURL          string
	tokenURL       string
	http           *http.Client
	sem            chan struct{}
	attemptTimeout time.Duration
	maxRetries     int
	backoffBase    time.Duration
	log            *slog.Logger
}

func New(o Options) *Client {
	if o.MaxConcurrency < 1 {
		o.MaxConcurrency = 1
	}
	if o.AttemptTimeout <= 0 {
		o.AttemptTimeout = 15 * time.Second
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = 0
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	base := strings.TrimRight(o.BaseURL, "/")

	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          o.MaxConcurrency * 2,
		MaxIdleConnsPerHost:   o.MaxConcurrency, // por defecto sería 2: forzaría a reabrir conexiones
		MaxConnsPerHost:       o.MaxConcurrency,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &Client{
		baseURL:        base,
		wsURL:          base + "/webservice/rest/server.php",
		tokenURL:       base + "/login/token.php",
		http:           &http.Client{Transport: tr}, // sin Timeout global: se usa un contexto por intento
		sem:            make(chan struct{}, o.MaxConcurrency),
		attemptTimeout: o.AttemptTimeout,
		maxRetries:     o.MaxRetries,
		backoffBase:    200 * time.Millisecond,
		log:            o.Logger,
	}
}

// Call invoca una función del web service de Moodle y decodifica la respuesta JSON en out
// (out puede ser nil). idempotent indica si es seguro reintentar la llamada.
func (c *Client) Call(ctx context.Context, token, function string, params url.Values, idempotent bool, out any) error {
	form := make(url.Values, len(params)+3)
	for k, v := range params {
		form[k] = v
	}
	form.Set("wstoken", token)
	form.Set("wsfunction", function)
	form.Set("moodlewsrestformat", "json")
	body := form.Encode()

	attempts := 1
	if idempotent {
		attempts += c.maxRetries
	}

	var (
		raw []byte
		err error
	)
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if serr := sleep(ctx, c.backoff(i)); serr != nil {
				return serr
			}
			c.log.Debug("reintentando llamada a moodle", "function", function, "attempt", i+1, "error", err)
		}
		raw, err = c.post(ctx, c.wsURL, body)
		if err == nil || !isTransient(err) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("moodle %s: %w", function, err)
	}
	return decode(function, raw, out)
}

// RequestToken canjea usuario y contraseña de Moodle por un token (login/token.php).
// No se reintenta nunca, para no provocar bloqueos de cuenta por intentos fallidos.
// La contraseña solo viaja en el cuerpo de esta petición: no se registra ni se guarda.
func (c *Client) RequestToken(ctx context.Context, username, password, service string) (string, error) {
	form := url.Values{"username": {username}, "password": {password}, "service": {service}}
	raw, err := c.post(ctx, c.tokenURL, form.Encode())
	if err != nil {
		return "", fmt.Errorf("moodle login/token.php: %w", err)
	}
	var resp struct {
		Token     string `json:"token"`
		Error     string `json:"error"`
		ErrorCode string `json:"errorcode"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &resp); err != nil {
		return "", fmt.Errorf("moodle login/token.php: respuesta no válida: %w", err)
	}
	if resp.Token == "" {
		return "", &Error{Function: "login/token.php", Code: resp.ErrorCode, Message: resp.Error}
	}
	return resp.Token, nil
}

// Ping comprueba que el servidor Moodle responde (cualquier respuesta < 500).
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.baseURL+"/", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("moodle respondió %d", resp.StatusCode)
	}
	return nil
}

// post hace un único intento HTTP respetando el límite de concurrencia.
func (c *Client) post(ctx context.Context, endpoint, body string) ([]byte, error) {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", ErrOverloaded, ctx.Err())
	}

	actx, cancel := context.WithTimeout(ctx, c.attemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(actx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil { // el llamante canceló o venció su propio plazo: no es culpa de Moodle
			return nil, ctx.Err()
		}
		return nil, &transientError{err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &transientError{err: err}
	}
	if len(raw) > maxResponseBytes {
		return nil, errors.New("respuesta de moodle demasiado grande")
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return raw, nil
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return nil, &transientError{err: fmt.Errorf("http %d", resp.StatusCode)}
	default:
		return nil, fmt.Errorf("moodle respondió http %d", resp.StatusCode)
	}
}

// decode detecta excepciones de Moodle (HTTP 200 con {"exception": ...}) y decodifica out.
func decode(function string, raw []byte, out any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '{' {
		var e struct {
			Exception string `json:"exception"`
			ErrorCode string `json:"errorcode"`
			Message   string `json:"message"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Exception != "" {
			return &Error{Function: function, Code: e.ErrorCode, Message: e.Message}
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("moodle %s: respuesta no válida: %w", function, err)
	}
	return nil
}

// backoff: exponencial con jitter, tope 5 s.
func (c *Client) backoff(attempt int) time.Duration {
	d := c.backoffBase << (attempt - 1)
	if d <= 0 || d > 5*time.Second {
		d = 5 * time.Second
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
