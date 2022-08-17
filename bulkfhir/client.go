// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package bulkfhir helps manage communication and with bulk fhir APIs. At the
// moment, much of this package is still geared around the BCDA API, but is
// in the process of being generalized further.
package bulkfhir

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/medical_claims_tools/fhir"
)

var (
	// ErrorUnimplemented indicates that this method is currently unimplemented.
	ErrorUnimplemented = errors.New("method not implemented yet")
	// ErrorUnableToParseProgress is an error returned when GetJobStatus is unable
	// to parse the progress in the server response.
	ErrorUnableToParseProgress = errors.New("unable to parse progress out of X-Progress header")
	// ErrorUnauthorized indicates that the server considers this client
	// unauthorized (it is possible the token has expired). The caller of the
	// library should consider calling Authenticate() and then retrying the
	// operation if needed.
	ErrorUnauthorized = errors.New("server indicates this client is unauthorized")
	// ErrorTimeout indicates the operation timed out.
	ErrorTimeout = errors.New("this operation timed out")
	// ErrorUnexpectedStatusCode indicates an unexpected status code was present.
	ErrorUnexpectedStatusCode = errors.New("unexpected non-ok or non-accecpted HTTP status code")
	// ErrorGreaterThanOneContentLocation indicates more than 1 Content-Location header was present.
	ErrorGreaterThanOneContentLocation = errors.New("greater than 1 Content-Location header")
	// ErrorUnexpectedNumberOfXProgress indicated unexpected number of X-Progress headers present.
	ErrorUnexpectedNumberOfXProgress = errors.New("unexpected number of x-progress headers")
	// ErrorRetryableHTTPStatus may be wrapped into other errors emitted by this package
	// to indicate to the caller that a retryable http error code was returned
	// from the server.
	// TODO(b/239596656): consider adding auto-retry logic within this package.
	ErrorRetryableHTTPStatus = errors.New("this is a retryable but unexpected HTTP status code error")
)

// ExportGroupAll is a default group ID of "all" which can be supplied to
// StartBulkDataExport. Depending on your FHIR server, the all patients group
// ID may differ, so be sure to consult relevant documentation.
var ExportGroupAll = "all"

// TODO(b/239856442): generalize further to support additional resource types.

// ResourceType represents a FHIR resource that can be retrieved from the BCDA API.
type ResourceType int

const (
	// Patient represents a Patient FHIR resource type.
	Patient ResourceType = iota
	// Coverage represents a Coverage FHIR resource type.
	Coverage
	// ExplanationOfBenefit represents an ExplanationOfBenefit FHIR resource type.
	ExplanationOfBenefit
	// OperationOutcome is a FHIR resource type describing data that the BCD API
	// can't export due to errors.
	OperationOutcome
)

// AllResourceTypes is a ResourceType slice that will represent all ResourceTypes.
var AllResourceTypes = []ResourceType{Patient, Coverage, ExplanationOfBenefit}

// ToAPI returns the string BCDA API representation of the ResourceType.
func (r ResourceType) ToAPI() (string, error) {
	switch r {
	case Patient:
		return "Patient", nil
	case Coverage:
		return "Coverage", nil
	case ExplanationOfBenefit:
		return "ExplanationOfBenefit", nil
	case OperationOutcome:
		return "OperationOutcome", nil
	}
	return "", errors.New("the specified resource type does not have a mapped BCDA API value")
}

func (r ResourceType) String() string {
	s, err := r.ToAPI()
	if err != nil {
		return fmt.Sprintf("INVALID(%d)", int(r))
	}
	return s
}

// ResourceTypeFromAPI converts the API representation of a ResourceType to the internal enumerated representation.
func ResourceTypeFromAPI(r string) (ResourceType, error) {
	switch r {
	case "Patient":
		return Patient, nil
	case "Coverage":
		return Coverage, nil
	case "ExplanationOfBenefit":
		return ExplanationOfBenefit, nil
	case "OperationOutcome":
		return OperationOutcome, nil
	}
	return ResourceType(-1), errors.New("not a valid ResourceType")
}

// Client represents a BCDA API client at some API version.
type Client struct {
	baseURL string

	fullAuthURL string

	clientID     string
	clientSecret string
	authScopes   []string

	token      string
	httpClient *http.Client
}

// NewClient creates and returns a new bulk fhir API Client for the input
// baseURL. A full authentication endpoint to get a token must also be provided
// (this endpoint must include the baseURL component as well). authScopes
// is a set of scopes to be used alongside authentication requests (this can
// be empty if not needed for your FHIR server).
func NewClient(baseURL, fullAuthURL, clientID, clientSecret string, authScopes []string) (*Client, error) {
	return &Client{
		baseURL:      baseURL,
		fullAuthURL:  fullAuthURL,
		httpClient:   &http.Client{},
		clientID:     clientID,
		clientSecret: clientSecret,
		authScopes:   authScopes,
	}, nil
}

// Close is a placeholder for any cleanup actions needed for the Client. Please
// call this when finished with a Client.
func (c *Client) Close() error { return nil }

// Header constants
const (
	acceptHeader         = "Accept"
	acceptHeaderJSON     = "application/json"
	acceptHeaderFHIRJSON = "application/fhir+json"

	contentTypeHeader         = "Content-Type"
	contentTypeFormURLEncoded = "application/x-www-form-urlencoded"

	preferHeader      = "Prefer"
	preferHeaderAsync = "respond-async"

	authorizationHeader = "Authorization"

	contentLocation = "Content-Location"

	xProgress = "X-Progress"
)

// Endpoint locations
const (
	bulkDataExportEndpointFmtStr = "/Group/%s/$export"
)

// progressREGEX matches strings like "(50%)" and captures the percentile number (50).
var progressREGEX = regexp.MustCompile(`\(([0-9]+?)%\)`)

// Authenticate authenticates with the bulk fhir API to fetch a JSON Web Token to use for this
// session. The token is returned, but also stored in the client to automatically attach to future
// API requests where needed.
//
// Authenticate must be called before calling other methods in the Client, otherwise the methods
// will return an error that indicates Authenticate has not yet been called.
func (c *Client) Authenticate() (token string, err error) {
	url := c.fullAuthURL

	body := buildAuthBody(c.authScopes)
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return "", err
	}

	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Add(acceptHeader, acceptHeaderJSON)
	if body != nil {
		req.Header.Add(contentTypeHeader, contentTypeFormURLEncoded)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}

	var tr tokenResponse

	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&tr); err != nil {
		return "", err
	}

	c.token = tr.Token

	return tr.Token, nil
}

// StartBulkDataExport starts a job via the bulk fhir API to begin exporting the
// requested resource types since the provided timestamp for the provided group,
// and returns the URL to query the job status (from the response Content-
// Location header). The variable bulkfhir.ExportGroupAll can be provided
// for the group parameter if you wish to retrieve all FHIR resources.
func (c *Client) StartBulkDataExport(types []ResourceType, since time.Time, groupID string) (jobStatusURL string, err error) {
	if len(c.token) == 0 {
		return "", ErrorUnauthorized
	}

	u, err := url.Parse(c.baseURL + fmt.Sprintf(bulkDataExportEndpointFmtStr, groupID))
	if err != nil {
		return "", err
	}
	qParams := u.Query()

	if !since.IsZero() {
		qParams.Add("_since", fhir.ToFHIRInstant(since))
	}

	if len(types) > 0 {
		v, err := resourceTypesToQueryValue(types)
		if err != nil {
			return "", err
		}
		qParams.Add("_type", v)
	}

	u.RawQuery = qParams.Encode()
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}

	req.Header.Add(acceptHeader, acceptHeaderFHIRJSON)
	req.Header.Add(preferHeader, preferHeaderAsync)
	req.Header.Add(authorizationHeader, fmt.Sprintf("Bearer %s", c.token))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return "", ErrorUnauthorized
	}
	// TODO(b/163811116): revisit possibly accecpting other 2xx status codes
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("unexpected non-OK and non-Accepted http status code: %d %w", resp.StatusCode, ErrorUnexpectedStatusCode)
	}

	// Extract the URL location used to check job status
	cLocations := resp.Header.Values(contentLocation)
	if len(cLocations) != 1 {
		return "", fmt.Errorf("one Content-Location header value expected. Instead got: %d %w", len(cLocations), ErrorGreaterThanOneContentLocation)
	}

	return cLocations[0], nil
}

// JobStatus represents the current status of a bulk fhir export Job, returned from GetJobStatus.
type JobStatus struct {
	IsComplete      bool
	PercentComplete int
	// ResultURLs holds the final NDJSON URLs for the job by ResourceType (if the job is complete).
	ResultURLs map[ResourceType][]string
	// Indicates the FHIR server time when the bulk data export was processed.
	TransactionTime time.Time
}

// JobStatus retrieves the current JobStatus via the bulk fhir API for the
// provided job status URL.
func (c *Client) JobStatus(jobStatusURL string) (st JobStatus, err error) {
	if len(c.token) == 0 {
		return JobStatus{}, ErrorUnauthorized
	}

	req, err := http.NewRequest(http.MethodGet, jobStatusURL, nil)
	if err != nil {
		return JobStatus{}, err
	}
	req.Header.Add(authorizationHeader, fmt.Sprintf("Bearer %s", c.token))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return JobStatus{}, nil
	}

	switch resp.StatusCode {
	case http.StatusAccepted:
		// Job is still pending, check X-Progress header for progress information.
		p := resp.Header.Values(xProgress)
		if len(p) != 1 {
			return JobStatus{IsComplete: false}, fmt.Errorf("one X-Progress header value expected. Instead got: %d %w", len(p), ErrorUnexpectedNumberOfXProgress)
		}
		match := progressREGEX.FindStringSubmatch(p[0])
		if len(match) == 0 {
			return JobStatus{IsComplete: false}, ErrorUnableToParseProgress
		}
		progress, err := strconv.Atoi(match[1])
		if err != nil {
			return JobStatus{IsComplete: false}, err
		}
		return JobStatus{IsComplete: false, PercentComplete: progress}, nil

	case http.StatusOK:
		// Job is finished, NDJSON is ready for download.
		jobStatus := JobStatus{IsComplete: true, ResultURLs: make(map[ResourceType][]string)}
		var jr jobStatusResponse

		dec := json.NewDecoder(resp.Body)
		if err := dec.Decode(&jr); err != nil {
			return jobStatus, err
		}

		for _, item := range jr.Output {
			r, err := ResourceTypeFromAPI(item.ResourceType)
			if err != nil {
				return JobStatus{}, err
			}
			jobStatus.ResultURLs[r] = append(jobStatus.ResultURLs[r], item.URL)
		}

		t, err := fhir.ParseFHIRInstant(jr.TransactionTime)
		if err != nil {
			return JobStatus{}, err
		}
		jobStatus.TransactionTime = t

		return jobStatus, nil
	case http.StatusUnauthorized:
		return JobStatus{}, ErrorUnauthorized
	default:
		return JobStatus{}, fmt.Errorf("unexpected non-OK http status code: %d %w", resp.StatusCode, ErrorUnexpectedStatusCode)
	}
}

// MonitorResult holds either a JobStatus or an error.
type MonitorResult struct {
	// Status holdes the JobStatus
	Status JobStatus
	// Error holds an error associated with this entry (if any)
	Error error
}

// MonitorJobStatus will asynchronously check the status of job at the
// provided checkPeriod until either the job completes or until the timeout.
// Each time the job status is checked, a MonitorResult will be emitted to
// the returned channel for the caller to consume. When the timeout is reached
// or the job is completed, the final completed JobStatus will be sent to the
// channel (or the ErrorTimeout error), and the channel will be closed.
// If an ErrorUnauthroized is encountered, MonitorJobStatus will attempt to
// reauthenticate and continue trying.
func (c *Client) MonitorJobStatus(jobStatusURL string, checkPeriod, timeout time.Duration) <-chan *MonitorResult {
	out := make(chan *MonitorResult, 100)
	deadline := time.Now().Add(timeout)
	go func() {
		var jobStatus JobStatus
		var err error
		for !jobStatus.IsComplete && time.Now().Before(deadline) {
			jobStatus, err = c.JobStatus(jobStatusURL)
			if err != nil {
				if errors.Is(err, ErrorUnauthorized) {
					_, err = c.Authenticate()
					if err != nil {
						out <- &MonitorResult{Error: err}
					}
					continue
				}
				out <- &MonitorResult{Error: err}
			} else {
				out <- &MonitorResult{Status: jobStatus}
			}

			if !jobStatus.IsComplete {
				time.Sleep(checkPeriod)
			}
		}
		if !jobStatus.IsComplete {
			out <- &MonitorResult{Error: ErrorTimeout}
		}
		close(out)
	}()
	return out
}

// GetData retrieves the NDJSON data result from the provided BCDA result url.
// The caller must close the dataStream io.ReadCloser when finished.
func (c *Client) GetData(bcdaURL string) (dataStream io.ReadCloser, err error) {
	if len(c.token) == 0 {
		return nil, ErrorUnauthorized
	}

	req, err := http.NewRequest(http.MethodGet, bcdaURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add(authorizationHeader, fmt.Sprintf("Bearer %s", c.token))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	// TODO(b/163811116): revisit possibly accecpting other 2xx status codes
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	// Handle some explicit error cases
	case http.StatusUnauthorized:
		return nil, ErrorUnauthorized
	case http.StatusNotFound:
		// BCDA 404s need to be retried in some instances.
		return nil, retryableNonOKError(resp.StatusCode)
	default:
		return nil, fmt.Errorf("unexpected non-OK http status code: %d %w", resp.StatusCode, ErrorUnexpectedStatusCode)
	}
}

func retryableNonOKError(code int) error {
	return fmt.Errorf("unexpected non-OK http status code: %d %w", code, ErrorRetryableHTTPStatus)
}

// tokenResponse represents the BCDA api response from the GetToken endpoint.
type tokenResponse struct {
	Token string `json:"access_token"`
}

// jobStatusResponse represents the BCDA api response from the JobStatus endpoint.
type jobStatusResponse struct {
	Output          []jobStatusOutput `json:"output"`
	TransactionTime string            `json:"transactionTime"`
}

type jobStatusOutput struct {
	ResourceType string `json:"type"`
	URL          string `json:"url"`
}

// resourceTypestoQueryValue takes a slice of ResourceType and converts it into a query string value
// that can be sent to the bulk fhir API.
//
// For example [ExplanationOfBenefit, Patient] would result in "ExplanationOfBenefit,Patient"
func resourceTypesToQueryValue(types []ResourceType) (string, error) {
	v, err := types[0].ToAPI()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(v)
	for _, t := range types[1:] {
		a, err := t.ToAPI()
		if err != nil {
			return "", err
		}
		b.WriteString(",")
		b.WriteString(a)
	}
	return b.String(), nil
}

// buildAuthBody serializes the provided slice of scopes for use in
// Authenticate's HTTP body using the expected urlencoded scheme, and adds in
// the default grant_type.
func buildAuthBody(scopes []string) io.Reader {
	if len(scopes) == 0 {
		return nil
	}

	s := strings.Builder{}
	// Add all scopes with a trailing space to the builder, except the last scope
	// for which a trailing space is not included.
	for _, scope := range scopes[0 : len(scopes)-1] {
		s.WriteString(scope + " ")
	}
	s.WriteString(scopes[len(scopes)-1]) // write last element with trailing space.

	v := url.Values{}
	v.Add("scope", s.String())
	v.Add("grant_type", "client_credentials")

	return bytes.NewBufferString(v.Encode())
}