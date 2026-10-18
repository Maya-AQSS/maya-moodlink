package moodle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const pluginFileFunction = "webservice/pluginfile.php"

var (
	// ErrInvalidFileURL: la URL no es un fichero de este Moodle servido por webservice/pluginfile.php.
	ErrInvalidFileURL = errors.New("moodle: la URL no es un fichero de Moodle válido")
	// ErrFileTooLarge: el fichero supera el tamaño máximo de descarga.
	ErrFileTooLarge = errors.New("moodle: el fichero supera el tamaño máximo permitido")
)

// File es un fichero descargado de Moodle.
type File struct {
	ContentType string
	Data        []byte
}

// DownloadFile descarga un fichero de Moodle a partir de la file_url que devuelven los web
// services (p. ej. los ficheros de una entrega). maxBytes limita el tamaño del fichero.
//
//   - Solo se aceptan URLs de webservice/pluginfile.php del propio Moodle. La URL la manda
//     quien llama: sin esta comprobación se podría enviar el token a cualquier servidor.
//   - El token va en el cuerpo (POST), nunca en la URL, como en Call.
//   - El fichero se lee entero en memoria antes de devolverlo. Así Auth.Do puede renovar el
//     token y repetir la descarga sin haber enviado nada todavía a quien llama.
//   - No hay timeout por intento: la descarga puede tardar más que una llamada normal. La
//     limita el contexto de la petición (REQUEST_TIMEOUT).
func (c *Client) DownloadFile(ctx context.Context, token, fileURL string, maxBytes int64) (*File, error) {
	target, err := c.pluginFileURL(fileURL)
	if err != nil {
		return nil, err
	}
	body := url.Values{"token": {token}}.Encode()

	var f *File
	for i := 0; i <= c.maxRetries; i++ {
		if i > 0 {
			if serr := sleep(ctx, c.backoff(i)); serr != nil {
				return nil, serr
			}
			c.log.Debug("reintentando descarga de moodle", "attempt", i+1, "error", err)
		}
		f, err = c.fetchFile(ctx, target, body, maxBytes)
		if err == nil || !isTransient(err) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("moodle %s: %w", pluginFileFunction, err)
	}
	return f, nil
}

// pluginFileURL comprueba que fileURL apunta a webservice/pluginfile.php de este Moodle y
// devuelve la URL sin query ni fragmento.
func (c *Client) pluginFileURL(fileURL string) (string, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(fileURL)
	if err != nil || u.User != nil || u.Scheme != base.Scheme || u.Host != base.Host {
		return "", ErrInvalidFileURL
	}
	prefix := strings.TrimRight(base.Path, "/") + "/webservice/pluginfile.php/"
	if !strings.HasPrefix(u.Path, prefix) || len(u.Path) == len(prefix) || strings.Contains(u.Path, "/../") || strings.HasSuffix(u.Path, "/..") {
		return "", ErrInvalidFileURL
	}
	return base.Scheme + "://" + base.Host + u.EscapedPath(), nil
}

// fetchFile hace un único intento de descarga respetando el límite de concurrencia.
func (c *Client) fetchFile(ctx context.Context, target, body string, maxBytes int64) (*File, error) {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", ErrOverloaded, ctx.Err())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &transientError{err: err}
	}
	defer resp.Body.Close()

	if resp.ContentLength > maxBytes {
		return nil, ErrFileTooLarge
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &transientError{err: err}
	}

	// pluginfile.php es un AJAX_SCRIPT: sus excepciones llegan como JSON
	// ({"error": ..., "errorcode": ...}), a veces con HTTP 200. Un fichero de verdad siempre
	// lleva Content-Disposition, así que un JSON sin ella es un error de Moodle.
	if resp.Header.Get("Content-Disposition") == "" {
		if me := pluginFileError(raw); me != nil {
			return nil, me
		}
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, &Error{Function: pluginFileFunction, Code: "filenotfound", Message: "el fichero no existe"}
	case http.StatusForbidden:
		return nil, &Error{Function: pluginFileFunction, Code: "nopermissions", Message: "sin permiso sobre el fichero"}
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return nil, &transientError{err: fmt.Errorf("http %d", resp.StatusCode)}
	default:
		return nil, fmt.Errorf("moodle respondió http %d", resp.StatusCode)
	}

	if int64(len(raw)) > maxBytes {
		return nil, ErrFileTooLarge
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	return &File{ContentType: ct, Data: raw}, nil
}

// pluginFileError devuelve el error de Moodle si raw es una excepción en JSON, o nil.
func pluginFileError(raw []byte) *Error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var e struct {
		Error     string `json:"error"`
		Message   string `json:"message"`
		ErrorCode string `json:"errorcode"`
	}
	if json.Unmarshal(raw, &e) != nil || e.ErrorCode == "" {
		return nil
	}
	msg := e.Error
	if msg == "" {
		msg = e.Message
	}
	return &Error{Function: pluginFileFunction, Code: e.ErrorCode, Message: msg}
}
