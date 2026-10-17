package moodle

import (
	"context"
	"net/url"
	"strconv"
)

// SubmissionFile es un fichero adjunto a una entrega (plugin "file", área "submission_files").
// FileURL apunta a webservice/pluginfile.php y solo sirve acompañada de un token: quien la
// reciba no puede descargar el fichero por sí mismo.
type SubmissionFile struct {
	Filename     string `json:"filename"`
	Filepath     string `json:"filepath"`
	Filesize     int64  `json:"filesize"`
	FileURL      string `json:"fileurl"`
	Mimetype     string `json:"mimetype"`
	TimeModified int64  `json:"timemodified"`
}

// Submission es la última entrega (último intento) de un usuario en una tarea.
type Submission struct {
	ID            int    `json:"id"`
	UserID        int    `json:"userid"`
	AttemptNumber int    `json:"attemptnumber"` // empieza en 0
	TimeCreated   int64  `json:"timecreated"`
	TimeModified  int64  `json:"timemodified"`
	Status        string `json:"status"` // new, draft, submitted, reopened
	GradingStatus string `json:"gradingstatus"`
	Plugins       []struct {
		Type      string `json:"type"`
		FileAreas []struct {
			Area  string           `json:"area"`
			Files []SubmissionFile `json:"files"`
		} `json:"fileareas"`
	} `json:"plugins"`
}

// Files devuelve los ficheros que el alumno adjuntó a la entrega.
func (s *Submission) Files() []SubmissionFile {
	var files []SubmissionFile
	for _, p := range s.Plugins {
		if p.Type != "file" {
			continue
		}
		for _, fa := range p.FileAreas {
			if fa.Area == "submission_files" {
				files = append(files, fa.Files...)
			}
		}
	}
	return files
}

type warning struct {
	Item        string `json:"item"`
	ItemID      int    `json:"itemid"`
	WarningCode string `json:"warningcode"`
	Message     string `json:"message"`
}

// AssignSubmissions devuelve la última entrega de cada usuario en la tarea (mod_assign_get_submissions).
// status filtra por estado de la entrega ("" = todas).
//
// Moodle no lanza excepción si la tarea no existe o no hay permiso sobre ella: simplemente no
// la incluye en la respuesta (en el segundo caso, con un warning). Se traduce a *Error con los
// códigos que ya entiende quien llama: "nopermissions" o "invalidrecord".
func (c *Client) AssignSubmissions(ctx context.Context, token string, assignmentID int, status string) ([]Submission, error) {
	const function = "mod_assign_get_submissions"
	params := url.Values{"assignmentids[0]": {strconv.Itoa(assignmentID)}}
	if status != "" {
		params.Set("status", status)
	}

	var resp struct {
		Assignments []struct {
			AssignmentID int          `json:"assignmentid"`
			Submissions  []Submission `json:"submissions"`
		} `json:"assignments"`
		Warnings []warning `json:"warnings"`
	}
	if err := c.Call(ctx, token, function, params, true, &resp); err != nil {
		return nil, err
	}
	for _, a := range resp.Assignments {
		if a.AssignmentID == assignmentID {
			return a.Submissions, nil
		}
	}
	for _, w := range resp.Warnings {
		if w.ItemID == assignmentID {
			return nil, &Error{Function: function, Code: "nopermissions", Message: w.Message}
		}
	}
	return nil, &Error{Function: function, Code: "invalidrecord", Message: "la tarea no existe o no tiene entregas visibles"}
}
