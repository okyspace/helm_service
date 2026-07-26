package api

import (
	"time"

	release "helm.sh/helm/v4/pkg/release/v1"
)

// ChartView is a trimmed view of chart metadata: the full chart object
// carries every template file and is not useful (or safe size-wise) to
// return over the wire.
type ChartView struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	AppVersion string `json:"appVersion,omitempty"`
}

// ReleaseView is a trimmed view of a Helm release, returned by install,
// upgrade, status, and template.
type ReleaseView struct {
	Name         string    `json:"name"`
	Namespace    string    `json:"namespace"`
	Revision     int       `json:"revision"`
	Status       string    `json:"status,omitempty"`
	Description  string    `json:"description,omitempty"`
	Notes        string    `json:"notes,omitempty"`
	Manifest     string    `json:"manifest,omitempty"`
	Chart        ChartView `json:"chart"`
	LastDeployed time.Time `json:"lastDeployed,omitzero"`
}

func toReleaseView(rel *release.Release) ReleaseView {
	v := ReleaseView{
		Name:      rel.Name,
		Namespace: rel.Namespace,
		Revision:  rel.Version,
		Manifest:  rel.Manifest,
	}
	if rel.Info != nil {
		v.Status = string(rel.Info.Status)
		v.Description = rel.Info.Description
		v.Notes = rel.Info.Notes
		v.LastDeployed = rel.Info.LastDeployed
	}
	if rel.Chart != nil && rel.Chart.Metadata != nil {
		v.Chart = ChartView{
			Name:       rel.Chart.Metadata.Name,
			Version:    rel.Chart.Metadata.Version,
			AppVersion: rel.Chart.Metadata.AppVersion,
		}
	}
	return v
}

// errorResponse is the JSON body written for any non-2xx response.
type errorResponse struct {
	Error string `json:"error"`
}

// versionResponse is the JSON body returned by GET /version.
type versionResponse struct {
	Version string `json:"version"`
}
