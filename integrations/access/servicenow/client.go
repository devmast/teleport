/*
Copyright 2015-2023 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package servicenow

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go/aws/defaults"
	"github.com/go-resty/resty/v2"
	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/integrations/access/common"
	"github.com/gravitational/teleport/integrations/lib"
	"github.com/gravitational/teleport/integrations/lib/logger"
)

const (
	// DateTimeFormat is the time format used by servicenow
	DateTimeFormat = "2006-01-02 15:04:05"
)

// Client is a wrapper around resty.Client.
type Client struct {
	ClientConfig

	client *resty.Client
}

// ClientConfig is the config for the servicenow client.
type ClientConfig struct {
	// APIEndpoint is the endpoint for the Servicenow API
	// api url of the form  https://instance.service-now.com/ with optional trailing '/'
	APIEndpoint string

	// WebProxyURL is the Teleport address used when building the bodies of the incidents
	// allowing links to the access requests to be built
	WebProxyURL *url.URL

	// ClusterName is the name of the Teleport cluster.
	ClusterName string

	// Username is the username used by the client for basic auth.
	Username string
	// APIToken is the token used for basic auth.
	APIToken string
	// CloseCode is the ServiceNow close code that incidents will be closed with.
	CloseCode string

	// StatusSink receives any status updates from the plugin for
	// further processing. Status updates will be ignored if not set.
	StatusSink common.StatusSink
}

// NewClient creates a new Servicenow client for managing incidents.
func NewClient(conf ClientConfig) (*Client, error) {
	if err := conf.checkAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	client := resty.NewWithClient(defaults.Config().HTTPClient)
	apiURL, err := url.Parse(conf.APIEndpoint)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if apiURL.Scheme == "http" && apiURL.Hostname() != "127.0.0.1" {
		return nil, trace.BadParameter("http scheme is only permitted for localhost: %v", apiURL.Host)
	}
	if apiURL.Hostname() != "127.0.0.1" {
		apiURL.Scheme = "https"
	}

	client.SetBaseURL(conf.APIEndpoint).
		SetHeader("Content-Type", "application/json").
		SetHeader("Accept", "application/json").
		SetBasicAuth(conf.Username, conf.APIToken)
	return &Client{
		client:       client,
		ClientConfig: conf,
	}, nil
}

func (conf ClientConfig) checkAndSetDefaults() error {
	if conf.APIEndpoint == "" {
		return trace.BadParameter("missing required field: APIEndpoint")
	}
	return nil
}

func errWrapper(statusCode int, body string) error {
	switch statusCode {
	case http.StatusForbidden:
		return trace.AccessDenied("servicenow API access denied: status code %v: %q", statusCode, body)
	case http.StatusRequestTimeout:
		return trace.ConnectionProblem(nil, "request to servicenow API failed: status code %v: %q", statusCode, body)
	}
	return trace.Errorf("request to servicenow API failed: status code %d: %q", statusCode, body)
}

// CreateIncident creates an servicenow incident.
func (snc *Client) CreateIncident(ctx context.Context, reqID string, reqData RequestData) (Incident, error) {
	bodyDetails, err := snc.buildIncidentBody(snc.WebProxyURL, reqID, reqData)
	if err != nil {
		return Incident{}, trace.Wrap(err)
	}

	body := Incident{
		ShortDescription: fmt.Sprintf("Teleport access request from user %s", reqData.User),
		Description:      bodyDetails,
		Caller:           reqData.User,
	}

	if len(reqData.SuggestedReviewers) != 0 {
		// Only one assignee per incident allowed so just grab the first.
		body.AssignedTo = reqData.SuggestedReviewers[0]
	}

	var result incidentResult
	resp, err := snc.client.NewRequest().
		SetContext(ctx).
		SetBody(body).
		SetResult(&result).
		Post("/api/now/v1/table/incident")
	if err != nil {
		return Incident{}, trace.Wrap(err)
	}
	defer resp.RawResponse.Body.Close()
	if resp.IsError() {
		return Incident{}, errWrapper(resp.StatusCode(), string(resp.Body()))
	}

	return Incident{IncidentID: result.Result.IncidentID}, nil
}

// PostReviewNote posts a note once a new request review appears.
func (snc *Client) PostReviewNote(ctx context.Context, incidentID string, review types.AccessReview) error {
	note, err := snc.buildReviewNoteBody(review)
	if err != nil {
		return trace.Wrap(err)
	}
	body := Incident{
		WorkNotes: note,
	}
	resp, err := snc.client.NewRequest().
		SetContext(ctx).
		SetBody(body).
		SetPathParams(map[string]string{"sys_id": incidentID}).
		Patch("/api/now/v1/table/incident/{sys_id}")
	if err != nil {
		return trace.Wrap(err)
	}
	defer resp.RawResponse.Body.Close()
	if resp.IsError() {
		return errWrapper(resp.StatusCode(), string(resp.Body()))
	}
	return nil
}

// ResolveIncident resolves an incident and posts a note with resolution details.
func (snc *Client) ResolveIncident(ctx context.Context, incidentID string, resolution Resolution) error {
	note, err := snc.buildResolutionNoteBody(resolution)
	if err != nil {
		return trace.Wrap(err)
	}
	body := Incident{
		CloseCode:     snc.CloseCode,
		IncidentState: resolution.State,
		CloseNotes:    note,
	}
	resp, err := snc.client.NewRequest().
		SetContext(ctx).
		SetBody(body).
		SetPathParams(map[string]string{"sys_id": incidentID}).
		Patch("/api/now/v1/table/incident/{sys_id}")
	if err != nil {
		return trace.Wrap(err)
	}
	defer resp.RawResponse.Body.Close()
	if resp.IsError() {
		return errWrapper(resp.StatusCode(), string(resp.Body()))
	}
	return nil
}

// GetOnCall returns the current users on-call for the given rota ID.
func (snc *Client) GetOnCall(ctx context.Context, rotaID string) ([]string, error) {
	formattedTime := time.Now().Format(DateTimeFormat)
	var result onCallResult
	resp, err := snc.client.NewRequest().
		SetContext(ctx).
		SetQueryParams(map[string]string{
			"rota_ids":  rotaID,
			"date_time": formattedTime,
		}).
		SetResult(&result).
		Get("/api/now/on_call_rota/whoisoncall")
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer resp.RawResponse.Body.Close()
	if resp.IsError() {
		return nil, errWrapper(resp.StatusCode(), string(resp.Body()))
	}
	if len(result.Result) == 0 {
		return nil, trace.NotFound("no user found for given rota: %q", rotaID)
	}
	var emails []string
	for _, result := range result.Result {
		email, err := snc.GetUserEmail(ctx, result.UserID)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		emails = append(emails, email)
	}
	return emails, nil
}

// CheckHealth pings servicenow to check if it is reachable.
func (snc *Client) CheckHealth(ctx context.Context) error {
	resp, err := snc.client.NewRequest().
		SetContext(ctx).
		SetQueryParams(map[string]string{
			"sysparm_limit": "1",
		}).
		Get("/api/now/table/incident")
	if err != nil {
		return trace.Wrap(err)
	}
	defer resp.RawResponse.Body.Close()

	if snc.StatusSink != nil {
		var code types.PluginStatusCode
		switch {
		case resp.StatusCode() == http.StatusUnauthorized:
			code = types.PluginStatusCode_UNAUTHORIZED
		case resp.StatusCode() >= 200 && resp.StatusCode() < 400:
			code = types.PluginStatusCode_RUNNING
		default:
			code = types.PluginStatusCode_OTHER_ERROR
		}
		if err := snc.StatusSink.Emit(ctx, &types.PluginStatusV1{Code: code}); err != nil {
			log := logger.Get(resp.Request.Context())
			log.WithError(err).WithField("code", resp.StatusCode()).Errorf("Error while emitting servicenow plugin status: %v", err)
		}
	}

	if resp.IsError() {
		return errWrapper(resp.StatusCode(), string(resp.Body()))
	}
	return nil
}

// GetUserEmail returns the email address for the given user ID
func (snc *Client) GetUserEmail(ctx context.Context, userID string) (string, error) {
	var result userResult
	resp, err := snc.client.NewRequest().
		SetContext(ctx).
		SetQueryParams(map[string]string{
			"sysparm_fields": "email",
		}).
		SetPathParams(map[string]string{"user_id": userID}).
		SetResult(&result).
		Get("/api/now/table/sys_user/{user_id}")
	if err != nil {
		return "", trace.Wrap(err)
	}
	defer resp.RawResponse.Body.Close()
	if resp.IsError() {
		return "", errWrapper(resp.StatusCode(), string(resp.Body()))
	}
	if len(result.Result) == 0 {
		return "", trace.NotFound("no user found for given id")
	}
	if len(result.Result) != 1 {
		return "", trace.NotFound("more than one user returned for given id")
	}
	return result.Result[0].Email, nil
}

var (
	incidentWithRolesBodyTemplate = template.Must(template.New("incident body").Parse(
		`Teleport user {{.User}} submitted access request for roles {{range $index, $element := .Roles}}{{if $index}}, {{end}}{{ . }}{{end}} on Teleport cluster {{.ClusterName}}.
{{if .RequestReason}}Reason: {{.RequestReason}}{{end}}
{{if .RequestLink}}Click this link to review the request in Teleport: {{.RequestLink}}{{end}}
`,
	))
	incidentBodyTemplate = template.Must(template.New("incident body").Parse(
		`Teleport user {{.User}} submitted access request on Teleport cluster {{.ClusterName}}.
{{if .RequestReason}}Reason: {{.RequestReason}}{{end}}
{{if .RequestLink}}Click this link to review the request in Teleport: {{.RequestLink}}{{end}}
`,
	))
	reviewNoteTemplate = template.Must(template.New("review note").Parse(
		`{{.Author}} reviewed the request at {{.Created.Format .TimeFormat}}.
Resolution: {{.ProposedState}}.
{{if .Reason}}Reason: {{.Reason}}.{{end}}`,
	))
	resolutionNoteTemplate = template.Must(template.New("resolution note").Parse(
		`Access request has been {{.Resolution}}
{{if .ResolveReason}}Reason: {{.ResolveReason}}{{end}}`,
	))
)

func (snc *Client) buildIncidentBody(webProxyURL *url.URL, reqID string, reqData RequestData) (string, error) {
	var requestLink string
	if webProxyURL != nil {
		reqURL := *webProxyURL
		reqURL.Path = lib.BuildURLPath("web", "requests", reqID)
		requestLink = reqURL.String()
	}

	var builder strings.Builder
	template := incidentBodyTemplate
	if reqData.Resources == nil {
		template = incidentWithRolesBodyTemplate
	}
	err := template.Execute(&builder, struct {
		ID          string
		TimeFormat  string
		RequestLink string
		ClusterName string
		RequestData
	}{
		ID:          reqID,
		TimeFormat:  time.RFC822,
		RequestLink: requestLink,
		ClusterName: snc.ClusterName,
		RequestData: reqData,
	})
	if err != nil {
		return "", trace.Wrap(err)
	}
	return builder.String(), nil
}

func (snc *Client) buildReviewNoteBody(review types.AccessReview) (string, error) {
	var builder strings.Builder
	err := reviewNoteTemplate.Execute(&builder, struct {
		types.AccessReview
		ProposedState string
		TimeFormat    string
	}{
		review,
		review.ProposedState.String(),
		time.RFC822,
	})
	if err != nil {
		return "", trace.Wrap(err)
	}
	return builder.String(), nil
}

func (snc *Client) buildResolutionNoteBody(resolution Resolution) (string, error) {
	var builder strings.Builder
	err := resolutionNoteTemplate.Execute(&builder, struct {
		Resolution    string
		ResolveReason string
	}{
		Resolution:    snc.CloseCode,
		ResolveReason: resolution.Reason,
	})
	if err != nil {
		return "", trace.Wrap(err)
	}
	return builder.String(), nil
}
