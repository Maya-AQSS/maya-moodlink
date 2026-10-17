package server

import (
	"net/http"
	"strconv"

	"maya-moodlink/internal/moodle"
)

type submissionUser struct {
	ID        int    `json:"id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"firstname,omitempty"`
	LastName  string `json:"lastname,omitempty"`
	Email     string `json:"email,omitempty"`
}

type submissionFile struct {
	Filename     string `json:"filename"`
	Filepath     string `json:"filepath"`
	Filesize     int64  `json:"filesize"`
	Mimetype     string `json:"mimetype"`
	FileURL      string `json:"file_url"`
	TimeModified int64  `json:"time_modified"`
}

type submission struct {
	ID            int              `json:"id"`
	User          submissionUser   `json:"user"`
	AttemptNumber int              `json:"attempt_number"` // como en Moodle: el primer intento es 0
	Status        string           `json:"status"`
	GradingStatus string           `json:"grading_status"`
	TimeCreated   int64            `json:"time_created"`
	TimeModified  int64            `json:"time_modified"`
	Files         []submissionFile `json:"files"`
}

type submissionsResponse struct {
	AssignmentID int          `json:"assignment_id"`
	Count        int          `json:"count"`
	Submissions  []submission `json:"submissions"`
}

var submissionStatuses = map[string]bool{"": true, "new": true, "draft": true, "submitted": true, "reopened": true}

// GET /v1/assignments/{assignmentID}/submissions[?status=submitted]
//
// Última entrega de cada alumno en una tarea, con sus datos de usuario y los ficheros
// adjuntos. Las entregas de grupo (userid 0) se descartan. Es solo transporte: qué hacer con
// cada entrega lo decide Odoo.
func (s *Server) handleSubmissions(w http.ResponseWriter, r *http.Request) {
	// 1. Validación de la entrada: id de la tarea (ruta) y filtro opcional de estado (query).
	// Se rechaza antes de tocar Moodle para no gastar una llamada en una petición mal formada.
	assignmentID, err := strconv.Atoi(r.PathValue("assignmentID"))
	if err != nil || assignmentID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_assignment", "el id de la tarea debe ser un entero positivo")
		return
	}
	status := r.URL.Query().Get("status")
	if !submissionStatuses[status] {
		writeError(w, http.StatusBadRequest, "invalid_status", "status debe ser new, draft, submitted o reopened")
		return
	}

	// 2. Llamadas a Moodle con el token del usuario de X-Maya-User. Auth.Do decide el token
	// (profesor o cuenta de servicio) y, si es una cuenta de servicio y Moodle lo rechaza, lo
	// renueva y vuelve a ejecutar la función completa. Por eso las dos llamadas van dentro.
	var (
		subs  []moodle.Submission
		users map[int]moodle.User
	)
	err = s.Auth.Do(r.Context(), r.Header.Get(userHeader), func(token string) error {
		// 2a. Última entrega de cada usuario en la tarea
		var err error
		if subs, err = s.Moodle.AssignSubmissions(r.Context(), token, assignmentID, status); err != nil {
			return err
		}
		// 2b. En cada entrega está el id del usuario de moodle, lo cojo y pregunto a moodle por el resto de sus
		// datos (username = NIA, nombre...) en una sola llamada por lote,
		// en vez de una por entrega. Las entregas de grupo (userid 0) no tienen usuario.
		ids := make([]int, 0, len(subs))
		for _, sub := range subs {
			if sub.UserID > 0 {
				ids = append(ids, sub.UserID)
			}
		}
		// De cada us
		users, err = s.Moodle.UsersByID(r.Context(), token, ids)
		return err
	})
	// 3. Cualquier error de sesión o de Moodle se traduce a HTTP en un único sitio
	// (401 pedir token, 403 prohibido, 404 tarea inexistente, 502/503/504...).
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}

	// 4. Construcción de la respuesta: se aplana la estructura de Moodle (plugins/fileareas)
	// a un formato propio y estable, independiente de cómo lo devuelva Moodle.
	out := submissionsResponse{AssignmentID: assignmentID, Submissions: make([]submission, 0, len(subs))}
	for _, sub := range subs {
		// las entregas de grupo no se corresponden con ningún alumno: se descartan
		if sub.UserID <= 0 {
			continue
		}
		// si Moodle no devolvió el usuario (borrado o no visible) se envía solo su id
		u := users[sub.UserID]
		item := submission{
			ID: sub.ID,
			User: submissionUser{
				ID: sub.UserID, Username: u.Username, FirstName: u.FirstName, LastName: u.LastName, Email: u.Email,
			},
			AttemptNumber: sub.AttemptNumber,
			Status:        sub.Status,
			GradingStatus: sub.GradingStatus,
			TimeCreated:   sub.TimeCreated,
			TimeModified:  sub.TimeModified,
			Files:         []submissionFile{}, // [] y no null cuando no hay ficheros
		}
		// solo los ficheros adjuntos a la entrega (plugin file, área submission_files)
		for _, f := range sub.Files() {
			item.Files = append(item.Files, submissionFile{
				Filename: f.Filename, Filepath: f.Filepath, Filesize: f.Filesize,
				Mimetype: f.Mimetype, FileURL: f.FileURL, TimeModified: f.TimeModified,
			})
		}
		out.Submissions = append(out.Submissions, item)
	}
	// 5. count refleja las entregas devueltas (ya sin las de grupo), no las que dio Moodle
	out.Count = len(out.Submissions)
	writeJSON(w, http.StatusOK, out)
}
