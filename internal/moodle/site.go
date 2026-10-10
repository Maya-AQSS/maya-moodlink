package moodle

import "context"

// SiteInfo es la parte útil de core_webservice_get_site_info: sirve para validar un token
// y saber a qué usuario de Moodle pertenece.
type SiteInfo struct {
	SiteName string `json:"sitename"`
	Username string `json:"username"`
	FullName string `json:"fullname"`
	UserID   int    `json:"userid"`
	Release  string `json:"release"`
}

func (c *Client) SiteInfo(ctx context.Context, token string) (*SiteInfo, error) {
	var info SiteInfo

	// si hay err, se devuelve el error
	if err := c.Call(ctx, token, "core_webservice_get_site_info", nil, true, &info); err != nil {
		return nil, err
	}
	return &info, nil
}
