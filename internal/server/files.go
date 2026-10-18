package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"maya-moodlink/internal/moodle"
)

// defaultMaxDownloadBytes se usa si Deps.MaxDownloadBytes no está definido.
const defaultMaxDownloadBytes = 50 << 20 // 50 MiB

type downloadRequest struct {
	FileURL string `json:"file_url"`
}

// POST /v1/files/download   {"file_url": "<file_url de una entrega>"}
//
// Descarga un fichero de Moodle con el token del usuario de X-Maya-User y lo devuelve tal
// cual (cuerpo binario, con el Content-Type de Moodle). Odoo no puede descargarlo por sí
// mismo porque la file_url solo sirve acompañada de un token. La URL va en el cuerpo y no en
// la query para que no acabe en logs de proxies.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	// 1. Validación de la entrada antes de tocar Moodle
	var req downloadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil || req.FileURL == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "el cuerpo debe ser JSON con file_url")
		return
	}
	maxBytes := s.MaxDownloadBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxDownloadBytes
	}

	// 2. Descarga dentro de Auth.Do: si es una cuenta de servicio y Moodle rechaza el token,
	// se renueva y se repite la descarga
	var f *moodle.File
	err := s.Auth.Do(r.Context(), r.Header.Get(userHeader), func(token string) error {
		var err error
		f, err = s.Moodle.DownloadFile(r.Context(), token, req.FileURL, maxBytes)
		return err
	})
	switch {
	case errors.Is(err, moodle.ErrInvalidFileURL):
		writeError(w, http.StatusBadRequest, "invalid_file_url", "file_url no es un fichero de este Moodle (webservice/pluginfile.php)")
		return
	case errors.Is(err, moodle.ErrFileTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", "el fichero supera el tamaño máximo de descarga")
		return
	case err != nil:
		s.writeUpstreamError(w, err)
		return
	}

	// 3. El fichero, tal cual
	w.Header().Set("Content-Type", f.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(f.Data)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(f.Data)
}
