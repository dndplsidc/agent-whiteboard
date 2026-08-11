package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/webapi"
)

type jsonResource struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	ExpiresAt *int64 `json:"expires_at"`
	Permanent bool   `json:"permanent"`
}

type singleResourceOutput struct {
	SchemaVersion int          `json:"schema_version"`
	Resource      jsonResource `json:"resource"`
}

type multiResourceOutput struct {
	SchemaVersion int            `json:"schema_version"`
	Resources     []jsonResource `json:"resources"`
}

type markdownOutput struct {
	SchemaVersion int          `json:"schema_version"`
	Resource      jsonResource `json:"resource"`
	Markdown      string       `json:"markdown"`
	Context       string       `json:"context"`
}

type deleteOutput struct {
	SchemaVersion int `json:"schema_version"`
}

type daemonStatusOutput struct {
	SchemaVersion int  `json:"schema_version"`
	Installed     bool `json:"installed"`
	Loaded        bool `json:"loaded"`
	Running       bool `json:"running"`
	PID           int  `json:"pid,omitempty"`
}

type trustedOriginsOutput struct {
	SchemaVersion int      `json:"schema_version"`
	Origins       []string `json:"origins"`
}

type jsonErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type jsonErrorOutput struct {
	SchemaVersion int           `json:"schema_version"`
	Error         jsonErrorBody `json:"error"`
}

func resolveJSONResources(client Client, resources []webapi.Resource) ([]jsonResource, error) {
	resolved := make([]jsonResource, 0, len(resources))
	for _, resource := range resources {
		publicURL, err := client.PublicURL(resource.Path)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, jsonResource{
			ID: resource.ID, URL: publicURL, ExpiresAt: resource.ExpiresAt, Permanent: resource.Permanent,
		})
	}
	return resolved, nil
}

func writeResource(writer io.Writer, jsonMode bool, client Client, resource webapi.Resource) error {
	resolved, err := resolveJSONResources(client, []webapi.Resource{resource})
	if err != nil {
		return err
	}

	var output bytes.Buffer
	if jsonMode {
		if err := json.NewEncoder(&output).Encode(singleResourceOutput{SchemaVersion: 1, Resource: resolved[0]}); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(&output, resolved[0].URL)
	}
	_, err = writer.Write(output.Bytes())
	return err
}

func writeMarkdown(writer io.Writer, client Client, response webapi.MarkdownResponse) error {
	resolved, err := resolveJSONResources(client, []webapi.Resource{response.Resource})
	if err != nil {
		return err
	}

	var output bytes.Buffer
	if err := json.NewEncoder(&output).Encode(markdownOutput{
		SchemaVersion: 1,
		Resource:      resolved[0],
		Markdown:      response.Markdown,
		Context:       response.Context,
	}); err != nil {
		return err
	}
	_, err = writer.Write(output.Bytes())
	return err
}

func writeResourceList(writer io.Writer, jsonMode bool, client Client, resources []webapi.Resource) error {
	resolved, err := resolveJSONResources(client, resources)
	if err != nil {
		return err
	}

	var output bytes.Buffer
	if jsonMode {
		if err := json.NewEncoder(&output).Encode(multiResourceOutput{SchemaVersion: 1, Resources: resolved}); err != nil {
			return err
		}
	} else {
		for _, resource := range resolved {
			fmt.Fprintln(&output, resource.URL)
		}
	}
	_, err = writer.Write(output.Bytes())
	return err
}

func writeDeleteSuccess(writer io.Writer, jsonMode bool) error {
	if !jsonMode {
		return nil
	}
	return json.NewEncoder(writer).Encode(deleteOutput{SchemaVersion: 1})
}

func writeDaemonStatus(writer io.Writer, jsonMode bool, status common.LaunchAgentStatus) error {
	if (status.Running && (!status.Loaded || status.PID <= 0)) || (!status.Running && status.PID != 0) {
		return errors.New("launch agent manager returned invalid status")
	}
	if jsonMode {
		return json.NewEncoder(writer).Encode(daemonStatusOutput{
			SchemaVersion: 1,
			Installed:     status.Installed,
			Loaded:        status.Loaded,
			Running:       status.Running,
			PID:           status.PID,
		})
	}
	if _, err := fmt.Fprintf(writer, "installed: %t\nloaded: %t\nrunning: %t\n", status.Installed, status.Loaded, status.Running); err != nil {
		return err
	}
	if status.Running {
		_, err := fmt.Fprintf(writer, "pid: %d\n", status.PID)
		return err
	}
	return nil
}

func writeTrustedOrigins(writer io.Writer, jsonMode bool, origins []string) error {
	if jsonMode {
		if origins == nil {
			origins = []string{}
		}
		return json.NewEncoder(writer).Encode(trustedOriginsOutput{SchemaVersion: 1, Origins: origins})
	}
	for _, origin := range origins {
		if _, err := fmt.Fprintln(writer, origin); err != nil {
			return err
		}
	}
	return nil
}

func writeCommandError(stdout, stderr io.Writer, jsonMode bool, err error) {
	_ = stdout
	if common.IsNil(stderr) {
		stderr = io.Discard
	}
	err = stableCommandError(err)
	if jsonMode {
		_ = json.NewEncoder(stderr).Encode(jsonErrorOutput{
			SchemaVersion: 1,
			Error:         jsonErrorBody{Code: commandErrorCode(err), Message: commandErrorMessage(err)},
		})
		return
	}
	fmt.Fprintf(stderr, "Error: %s\n", commandErrorMessage(err))
}

func commandErrorCode(err error) string {
	if contextErr, contextOnly := contextOnlyError(err); contextOnly {
		if contextErr == context.DeadlineExceeded {
			return "timeout"
		}
		return "canceled"
	}
	var usage usageError
	if errors.As(err, &usage) {
		return string(common.CodeInvalidRequest)
	}
	var domainErr *common.Error
	if errors.As(err, &domainErr) {
		return string(domainErr.Code)
	}
	return string(common.CodeInternal)
}

func commandErrorMessage(err error) string {
	if errors.Is(err, common.ErrLaunchAgentUnsupported) {
		return common.ErrLaunchAgentUnsupported.Error()
	}
	if contextErr, contextOnly := contextOnlyError(err); contextOnly {
		if contextErr == context.DeadlineExceeded {
			return "request timed out"
		}
		return "request canceled"
	}
	var usage usageError
	if errors.As(err, &usage) {
		return usage.Error()
	}
	var domainErr *common.Error
	if errors.As(err, &domainErr) {
		return domainErr.Message
	}
	return "internal error"
}

func humanExpiration(expiresAt *int64) string {
	if expiresAt == nil {
		return "permanent"
	}
	value := time.Unix(*expiresAt, 0).UTC()
	if value.Year() < 0 || value.Year() > 9999 {
		return strconv.FormatInt(*expiresAt, 10)
	}
	return value.Format(time.RFC3339)
}
