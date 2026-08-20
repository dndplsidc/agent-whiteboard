package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	standardhttp "net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/edocsss/agent-whiteboard/internal/common"
	generalconfig "github.com/edocsss/agent-whiteboard/internal/config"
)

const (
	maxClientResponseBytes int64 = 1 << 20
	// Bound a default-size Markdown pair after worst-case JSON string escaping,
	// with space for the public resource envelope.
	maxWhiteboardResponseBytes int64 = 6*(generalconfig.DefaultMaxWhiteboardBytes+generalconfig.DefaultMaxContextBytes) + MultipartOverheadBytes
)

type ClientConfig struct {
	Server     string
	HTTPClient *standardhttp.Client
}

type File struct {
	Name   string
	Reader io.Reader
}

type multipartFile struct {
	fieldName string
	file      File
}

type WhiteboardKind string

const (
	WhiteboardMarkdown WhiteboardKind = "markdown"
	WhiteboardHTML     WhiteboardKind = "html"
)

type Client struct {
	baseURL    *url.URL
	httpClient *standardhttp.Client
}

func NewClient(config ClientConfig) (*Client, error) {
	if config.HTTPClient == nil {
		return nil, clientInvalidRequest("http client is required")
	}

	baseURL, err := url.Parse(config.Server)
	if err != nil || strings.Contains(config.Server, "#") || !validServerOrigin(baseURL) {
		return nil, clientInvalidRequest("server must be an absolute HTTP origin")
	}
	baseURL.Path = ""
	baseURL.RawPath = ""
	httpClient := *config.HTTPClient
	httpClient.CheckRedirect = func(*standardhttp.Request, []*standardhttp.Request) error {
		return standardhttp.ErrUseLastResponse
	}

	return &Client{baseURL: baseURL, httpClient: &httpClient}, nil
}

func (c *Client) CreateWhiteboard(
	ctx context.Context,
	kind WhiteboardKind,
	file File,
	expiresInSeconds *int64,
) (Resource, error) {
	if _, err := whiteboardEndpoint(kind); err != nil {
		return Resource{}, err
	}
	return Resource{}, clientInvalidRequest("creator context is required")
}

func (c *Client) CreateMarkdown(
	ctx context.Context,
	markdown File,
	creatorContext File,
	expiresInSeconds *int64,
) (Resource, error) {
	return c.createPairedWhiteboard(ctx, WhiteboardMarkdown, APIWhiteboardMarkdown, markdown, creatorContext, expiresInSeconds)
}

func (c *Client) CreateHTML(
	ctx context.Context,
	html File,
	creatorContext File,
	expiresInSeconds *int64,
) (Resource, error) {
	return c.createPairedWhiteboard(ctx, WhiteboardHTML, APIWhiteboardHTML, html, creatorContext, expiresInSeconds)
}

func (c *Client) createPairedWhiteboard(ctx context.Context, kind WhiteboardKind, endpoint string, source, creatorContext File, expiresInSeconds *int64) (Resource, error) {
	return c.createWhiteboard(ctx, kind, endpoint, []multipartFile{
		{fieldName: "file", file: source},
		{fieldName: "context", file: creatorContext},
	}, expiresInSeconds)
}

func (c *Client) createWhiteboard(
	ctx context.Context,
	kind WhiteboardKind,
	endpoint string,
	files []multipartFile,
	expiresInSeconds *int64,
) (Resource, error) {
	var response ResourceResponse
	err := c.doMultipart(ctx, standardhttp.MethodPost, endpoint, files, expiresInSeconds, standardhttp.StatusCreated, &response)
	if err != nil && response.Resource.ID == "" {
		return Resource{}, err
	}
	if validationErr := c.validateWhiteboardResource(response.Resource, kind, ""); validationErr != nil {
		return Resource{}, validationErr
	}
	return response.Resource, err
}

func (c *Client) UpdateWhiteboard(
	ctx context.Context,
	kind WhiteboardKind,
	id string,
	file File,
	expiresInSeconds *int64,
) (Resource, error) {
	if _, err := whiteboardEndpoint(kind); err != nil {
		return Resource{}, err
	}
	return Resource{}, clientInvalidRequest("creator context is required")
}

func (c *Client) UpdateMarkdown(
	ctx context.Context,
	id string,
	markdown File,
	creatorContext File,
	expiresInSeconds *int64,
) (Resource, error) {
	return c.updatePairedWhiteboard(ctx, WhiteboardMarkdown, APIWhiteboardMarkdown, id, markdown, creatorContext, expiresInSeconds)
}

func (c *Client) UpdateHTML(
	ctx context.Context,
	id string,
	html File,
	creatorContext File,
	expiresInSeconds *int64,
) (Resource, error) {
	return c.updatePairedWhiteboard(ctx, WhiteboardHTML, APIWhiteboardHTML, id, html, creatorContext, expiresInSeconds)
}

func (c *Client) updatePairedWhiteboard(ctx context.Context, kind WhiteboardKind, endpoint, id string, source, creatorContext File, expiresInSeconds *int64) (Resource, error) {
	return c.updateWhiteboard(ctx, kind, endpoint, id, []multipartFile{
		{fieldName: "file", file: source},
		{fieldName: "context", file: creatorContext},
	}, expiresInSeconds)
}

func (c *Client) updateWhiteboard(
	ctx context.Context,
	kind WhiteboardKind,
	endpoint string,
	id string,
	files []multipartFile,
	expiresInSeconds *int64,
) (Resource, error) {
	if err := common.ValidateID(id); err != nil {
		return Resource{}, err
	}

	var response ResourceResponse
	err := c.doMultipart(ctx, standardhttp.MethodPut, endpoint+"/"+url.PathEscape(id), files, expiresInSeconds, standardhttp.StatusOK, &response)
	if err != nil {
		return Resource{}, err
	}
	if err := c.validateWhiteboardResource(response.Resource, kind, id); err != nil {
		return Resource{}, err
	}
	return response.Resource, nil
}

func (c *Client) GetMarkdown(ctx context.Context, id string) (MarkdownResponse, error) {
	if err := common.ValidateID(id); err != nil {
		return MarkdownResponse{}, err
	}

	var encoded markdownResponseEnvelope
	if err := c.doWithResponseLimit(ctx, standardhttp.MethodGet, APIWhiteboardMarkdownResource+url.PathEscape(id), nil, "", standardhttp.StatusOK, &encoded, maxWhiteboardResponseBytes); err != nil {
		return MarkdownResponse{}, err
	}
	if encoded.Markdown == nil || *encoded.Markdown == "" || encoded.Context == nil {
		return MarkdownResponse{}, clientInvalidResponse("server returned an invalid response")
	}
	if err := c.validateResource(encoded.Resource, id); err != nil {
		return MarkdownResponse{}, err
	}
	if encoded.Resource.Type != string(WhiteboardMarkdown) || encoded.Resource.Path != PublicMarkdown+id {
		return MarkdownResponse{}, clientInvalidResponse("server returned an invalid response")
	}
	return MarkdownResponse{Resource: encoded.Resource, Markdown: *encoded.Markdown, Context: *encoded.Context}, nil
}

func (c *Client) GetHTML(ctx context.Context, id string) (HTMLResponse, error) {
	if err := common.ValidateID(id); err != nil {
		return HTMLResponse{}, err
	}

	var encoded htmlResponseEnvelope
	if err := c.doWithResponseLimit(ctx, standardhttp.MethodGet, APIWhiteboardHTMLResource+url.PathEscape(id), nil, "", standardhttp.StatusOK, &encoded, maxWhiteboardResponseBytes); err != nil {
		return HTMLResponse{}, err
	}
	if encoded.HTML == nil || *encoded.HTML == "" || encoded.Context == nil || *encoded.Context == "" {
		return HTMLResponse{}, clientInvalidResponse("server returned an invalid response")
	}
	if err := c.validateResource(encoded.Resource, id); err != nil {
		return HTMLResponse{}, err
	}
	if encoded.Resource.Type != string(WhiteboardHTML) || encoded.Resource.Path != PublicHTML+id {
		return HTMLResponse{}, clientInvalidResponse("server returned an invalid response")
	}
	return HTMLResponse{Resource: encoded.Resource, HTML: *encoded.HTML, Context: *encoded.Context}, nil
}

type markdownResponseEnvelope struct {
	Resource Resource
	Markdown *string
	Context  *string
}

type htmlResponseEnvelope struct {
	Resource Resource
	HTML     *string
	Context  *string
}

func (response *htmlResponseEnvelope) UnmarshalJSON(encoded []byte) error {
	type wireResponse struct {
		Resource Resource `json:"resource"`
		HTML     *string  `json:"html"`
		Context  *string  `json:"context"`
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var wire wireResponse
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid trailing JSON value")
	}
	*response = htmlResponseEnvelope(wire)
	return nil
}

func (response *markdownResponseEnvelope) UnmarshalJSON(encoded []byte) error {
	type wireResponse struct {
		Resource Resource `json:"resource"`
		Markdown *string  `json:"markdown"`
		Context  *string  `json:"context"`
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var wire wireResponse
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid trailing JSON value")
	}
	*response = markdownResponseEnvelope(wire)
	return nil
}

func (c *Client) DeleteWhiteboard(ctx context.Context, kind WhiteboardKind, id string) error {
	endpoint, err := whiteboardEndpoint(kind)
	if err != nil {
		return err
	}
	if err := common.ValidateID(id); err != nil {
		return err
	}
	return c.do(ctx, standardhttp.MethodDelete, endpoint+"/"+url.PathEscape(id), nil, "", standardhttp.StatusNoContent, nil)
}

func (c *Client) CreateImages(ctx context.Context, files []File, expiresInSeconds *int64) ([]Resource, error) {
	if len(files) == 0 {
		return nil, clientInvalidRequest("at least one image is required")
	}

	var response ImagesResponse
	err := c.doMultipart(ctx, standardhttp.MethodPost, APIImages, multipartFiles("images", files), expiresInSeconds, standardhttp.StatusCreated, &response)
	if err != nil {
		return nil, err
	}
	if response.Images == nil || len(response.Images) != len(files) {
		return nil, clientInvalidResponse("server returned an invalid response")
	}
	ids := make(map[string]struct{}, len(response.Images))
	for _, resource := range response.Images {
		if err := c.validateResource(resource, ""); err != nil {
			return nil, err
		}
		if _, duplicate := ids[resource.ID]; duplicate {
			return nil, clientInvalidResponse("server returned an invalid response")
		}
		ids[resource.ID] = struct{}{}
	}
	return response.Images, nil
}

func (c *Client) UpdateImage(
	ctx context.Context,
	id string,
	file File,
	expiresInSeconds *int64,
) (Resource, error) {
	if err := common.ValidateID(id); err != nil {
		return Resource{}, err
	}

	var response ResourceResponse
	err := c.doMultipart(ctx, standardhttp.MethodPut, APIImages+"/"+url.PathEscape(id), []multipartFile{{fieldName: "file", file: file}}, expiresInSeconds, standardhttp.StatusOK, &response)
	if err != nil {
		return Resource{}, err
	}
	if err := c.validateResource(response.Resource, id); err != nil {
		return Resource{}, err
	}
	return response.Resource, nil
}

func (c *Client) DeleteImage(ctx context.Context, id string) error {
	if err := common.ValidateID(id); err != nil {
		return err
	}
	return c.do(ctx, standardhttp.MethodDelete, APIImages+"/"+url.PathEscape(id), nil, "", standardhttp.StatusNoContent, nil)
}

func (c *Client) PublicURL(publicPath string) (string, error) {
	if c == nil || c.baseURL == nil {
		return "", clientInvalidRequest("client is not configured")
	}
	if publicPath == "" || strings.Contains(publicPath, "\\") || strings.Contains(publicPath, "#") {
		return "", clientInvalidRequest("invalid public path")
	}

	parsed, err := url.Parse(publicPath)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", clientInvalidRequest("invalid public path")
	}
	if strings.Contains(parsed.Path, "\\") {
		return "", clientInvalidRequest("invalid public path")
	}
	if !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") || path.Clean(parsed.Path) != parsed.Path {
		return "", clientInvalidRequest("invalid public path")
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return "", clientInvalidRequest("invalid public path")
		}
	}

	resolved := c.baseURL.ResolveReference(parsed)
	if resolved.Scheme != c.baseURL.Scheme || resolved.Host != c.baseURL.Host {
		return "", clientInvalidRequest("invalid public path")
	}
	return resolved.String(), nil
}

func (c *Client) validateWhiteboardResource(resource Resource, kind WhiteboardKind, expectedID string) error {
	if err := c.validateResource(resource, expectedID); err != nil {
		return err
	}
	var publicPath string
	switch kind {
	case WhiteboardMarkdown:
		publicPath = PublicMarkdown
	case WhiteboardHTML:
		publicPath = PublicHTML
	default:
		return clientInvalidRequest("unsupported whiteboard kind")
	}
	if resource.Type != string(kind) || resource.Path != publicPath+resource.ID {
		return clientInvalidResponse("server returned an invalid response")
	}
	return nil
}

func (c *Client) validateResource(resource Resource, expectedID string) error {
	if common.ValidateID(resource.ID) != nil || (expectedID != "" && resource.ID != expectedID) {
		return clientInvalidResponse("server returned an invalid response")
	}
	if resource.Permanent != (resource.ExpiresAt == nil) {
		return clientInvalidResponse("server returned an invalid response")
	}
	if _, err := c.PublicURL(resource.Path); err != nil {
		return clientInvalidResponse("server returned an invalid response")
	}
	return nil
}

func (c *Client) doMultipart(
	ctx context.Context,
	method string,
	endpoint string,
	files []multipartFile,
	expiresInSeconds *int64,
	wantStatus int,
	result any,
) error {
	if len(files) == 0 {
		return clientInvalidRequest("multipart files are required")
	}
	for _, upload := range files {
		if upload.fieldName == "" || upload.file.Name == "" || common.IsNil(upload.file.Reader) {
			return clientInvalidRequest("file field, name, and reader are required")
		}
	}

	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	request, err := c.newRequest(ctx, method, endpoint, reader)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return err
	}
	request.Header.Set("Content-Type", multipartWriter.FormDataContentType())

	go writeMultipart(writer, multipartWriter, files, expiresInSeconds)
	err = c.execute(request, wantStatus, result, maxClientResponseBytes)
	_ = reader.CloseWithError(err)
	return contextError(ctx, err)
}

func writeMultipart(
	pipe *io.PipeWriter,
	writer *multipart.Writer,
	files []multipartFile,
	expiresInSeconds *int64,
) {
	var writeErr error
	defer func() {
		if recover() != nil {
			writeErr = clientStreamError()
		}
		_ = pipe.CloseWithError(writeErr)
	}()
	for _, upload := range files {
		part, err := writer.CreateFormFile(upload.fieldName, upload.file.Name)
		if err != nil {
			writeErr = clientStreamError()
			break
		}
		if _, err := io.Copy(part, upload.file.Reader); err != nil {
			writeErr = clientStreamError()
			break
		}
	}
	if writeErr == nil && expiresInSeconds != nil {
		if err := writer.WriteField("expires_in_seconds", strconv.FormatInt(*expiresInSeconds, 10)); err != nil {
			writeErr = clientStreamError()
		}
	}
	if writeErr == nil {
		if err := writer.Close(); err != nil {
			writeErr = clientStreamError()
		}
	}
}

func multipartFiles(fieldName string, files []File) []multipartFile {
	uploads := make([]multipartFile, 0, len(files))
	for _, file := range files {
		uploads = append(uploads, multipartFile{fieldName: fieldName, file: file})
	}
	return uploads
}

func (c *Client) do(
	ctx context.Context,
	method string,
	endpoint string,
	body io.Reader,
	contentType string,
	wantStatus int,
	result any,
) error {
	return c.doWithResponseLimit(ctx, method, endpoint, body, contentType, wantStatus, result, maxClientResponseBytes)
}

func (c *Client) doWithResponseLimit(
	ctx context.Context,
	method string,
	endpoint string,
	body io.Reader,
	contentType string,
	wantStatus int,
	result any,
	successResponseLimit int64,
) error {
	request, err := c.newRequest(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	return contextError(ctx, c.execute(request, wantStatus, result, successResponseLimit))
}

func (c *Client) newRequest(ctx context.Context, method string, endpoint string, body io.Reader) (*standardhttp.Request, error) {
	if c == nil || c.baseURL == nil || c.httpClient == nil {
		return nil, clientInvalidRequest("client is not configured")
	}
	request, err := standardhttp.NewRequestWithContext(ctx, method, c.baseURL.String()+endpoint, body)
	if err != nil {
		return nil, contextError(ctx, err)
	}
	return request, nil
}

func (c *Client) execute(request *standardhttp.Request, wantStatus int, result any, successResponseLimit int64) error {
	response, err := c.httpClient.Do(request)
	if err != nil {
		err = contextError(request.Context(), err)
		var protocolErr *common.Error
		if errors.As(err, &protocolErr) {
			return protocolErr
		}
		return err
	}
	defer response.Body.Close()

	responseLimit := maxClientResponseBytes
	if response.StatusCode == wantStatus {
		responseLimit = successResponseLimit
	}
	body, err := readClientResponse(response.Body, responseLimit)
	if err != nil {
		return contextError(request.Context(), err)
	}
	if response.StatusCode != wantStatus {
		decoded, protocolErr := decodeClientErrorResponse(body)
		if protocolErr != nil {
			return protocolErr
		}
		if decoded.Resource != nil {
			if resourceResponse, ok := result.(*ResourceResponse); ok {
				resourceResponse.Resource = *decoded.Resource
			}
		}
		return common.NewError(decoded.Error.Code, decoded.Error.Message, nil)
	}
	if result == nil {
		if len(strings.TrimSpace(string(body))) != 0 {
			return clientInvalidResponse("server returned an invalid response")
		}
		return nil
	}
	if err := json.Unmarshal(body, result); err != nil {
		return clientInvalidResponse("server returned an invalid response")
	}
	return nil
}

func readClientResponse(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		return nil, clientInvalidResponse("could not read server response")
	}
	if int64(len(body)) > limit {
		return nil, clientInvalidResponse("server response is too large")
	}
	return body, nil
}

func decodeClientErrorResponse(body []byte) (ErrorResponse, error) {
	var response ErrorResponse
	if err := json.Unmarshal(body, &response); err != nil || !knownClientErrorCode(response.Error.Code) || response.Error.Message == "" {
		return ErrorResponse{}, clientInvalidResponse("server returned an invalid error response")
	}
	return response, nil
}

func knownClientErrorCode(code common.ErrorCode) bool {
	switch code {
	case common.CodeInvalidRequest,
		common.CodeNotFound,
		common.CodeContentTooLarge,
		common.CodeUnsupportedMediaType,
		common.CodeStorageUnavailable,
		common.CodeInternal:
		return true
	default:
		return false
	}
}

func whiteboardEndpoint(kind WhiteboardKind) (string, error) {
	switch kind {
	case WhiteboardMarkdown:
		return APIWhiteboardMarkdown, nil
	case WhiteboardHTML:
		return APIWhiteboardHTML, nil
	default:
		return "", clientInvalidRequest("invalid whiteboard kind")
	}
}

func validServerOrigin(server *url.URL) bool {
	if server == nil || (server.Scheme != "http" && server.Scheme != "https") || server.Host == "" {
		return false
	}
	if server.User != nil || server.Opaque != "" || server.RawQuery != "" || server.ForceQuery || server.Fragment != "" {
		return false
	}
	if server.Path != "" && server.Path != "/" {
		return false
	}
	return server.RawPath == "" || server.RawPath == "/"
}

func contextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
}

func clientInvalidRequest(message string) error {
	return common.NewError(common.CodeInvalidRequest, message, nil)
}

func clientInvalidResponse(message string) error {
	return common.NewError(common.CodeInternal, message, nil)
}

func clientStreamError() error {
	return clientInvalidResponse("could not stream request body")
}
