package model

type Labels map[string]string

type ClientPollRequest struct {
	Name   string `json:"name"`
	Labels Labels `json:"labels"`
}

func (c ClientPollRequest) GetName() string {
	return c.Name
}

func (c ClientPollRequest) GetLabels() map[string]string {
	return c.Labels
}
