package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// StepRegistry abstracts registry interaction
type StepRegistry interface {
	// GetStepVersion retrieves a step from the registry
	GetStepVersion(owner, name, version string) (*APIStepVersion, error)
}

// WerckerStepRegistry implements the StepRegistry interface to handle
type WerckerStepRegistry struct {
	baseURL string
	client  *http.Client
}

// NewWerckerStepRegistry creates a new instance of NewWerckerStepRegistry
func NewWerckerStepRegistry(baseURL string) StepRegistry {
	return &WerckerStepRegistry{
		baseURL: baseURL,
		client:  http.DefaultClient,
	}
}

// GetStepVersion retrieves a step from the registry
func (r *WerckerStepRegistry) GetStepVersion(owner, name, version string) (*APIStepVersion, error) {
	url := fmt.Sprintf("%s/api/steps/%s/%s/%s", r.baseURL, owner, name, version)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
		}
	}

	stepVersion := struct {
		Step struct {
			Summary    string `json:"summary"`
			TarballURL string `json:"tarballUrl"`
			Version    struct {
				Number string `json:"number"`
			} `json:"version"`
		} `json:"step"`
	}{}
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&stepVersion); err != nil {
		return nil, err
	}

	return &APIStepVersion{
		Description: stepVersion.Step.Summary,
		TarballURL:  stepVersion.Step.TarballURL,
		Version:     stepVersion.Step.Version.Number,
	}, nil
}
