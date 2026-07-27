package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	standardhttp "net/http"
	"strconv"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/common"
)

const (
	APIWhiteboardMarkdown         = "/api/v1/whiteboards/markdown"
	APIWhiteboardMarkdownResource = APIWhiteboardMarkdown + "/"
	APIWhiteboardHTML             = "/api/v1/whiteboards/html"
	APIImages                     = "/api/v1/images"
	PublicMarkdown                = "/whiteboards/markdown/"
	PublicHTML                    = "/whiteboards/html/"
	PublicHTMLContentSuffix       = "/content"
	PublicImages                  = "/images/"

	MultipartOverheadBytes  int64 = 64 << 10
	maxExpirationFieldBytes int64 = 20
)

type ErrorBody struct {
	Code    common.ErrorCode `json:"code"`
	Message string           `json:"message"`
}

type ErrorResponse struct {
	Error    ErrorBody `json:"error"`
	Resource *Resource `json:"resource,omitempty"`
}

type Resource struct {
	ID        string    `json:"id"`
	Type      string    `json:"type,omitempty"`
	Filename  string    `json:"filename,omitempty"`
	Extension string    `json:"extension,omitempty"`
	MediaType string    `json:"media_type,omitempty"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	ExpiresAt *int64    `json:"expires_at"`
	Permanent bool      `json:"permanent"`
}

type ResourceResponse struct {
	Resource Resource `json:"resource"`
}

type MarkdownResponse struct {
	Resource Resource `json:"resource"`
	Markdown string   `json:"markdown"`
	Context  string   `json:"context"`
}

type ImagesResponse struct {
	Images []Resource `json:"images"`
}

type Readiness interface {
	Ready(context.Context) error
}

type healthResponse struct {
	Status string `json:"status"`
}

type MultipartFile struct {
	FieldName string
	Filename  string
	Content   []byte
}

type MultipartForm struct {
	Files            []MultipartFile
	ExpiresInSeconds *int64
}

type MultipartFileLimit struct {
	FieldName string
	MaxBytes  int64
}

func WriteJSON(w standardhttp.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func RegisterHealth(mux *standardhttp.ServeMux, readiness Readiness) {
	mux.HandleFunc("GET /healthz", func(w standardhttp.ResponseWriter, r *standardhttp.Request) {
		if r.Method != standardhttp.MethodGet {
			w.Header().Set("Allow", standardhttp.MethodGet)
			w.WriteHeader(standardhttp.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		WriteJSON(w, standardhttp.StatusOK, healthResponse{Status: "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w standardhttp.ResponseWriter, r *standardhttp.Request) {
		if r.Method != standardhttp.MethodGet {
			w.Header().Set("Allow", standardhttp.MethodGet)
			w.WriteHeader(standardhttp.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if common.IsNil(readiness) || readiness.Ready(r.Context()) != nil {
			WriteJSON(w, standardhttp.StatusServiceUnavailable, healthResponse{Status: "unavailable"})
			return
		}
		WriteJSON(w, standardhttp.StatusOK, healthResponse{Status: "ready"})
	})
}

func WriteError(w standardhttp.ResponseWriter, err error) {
	status, body := publicError(err)
	WriteJSON(w, status, ErrorResponse{Error: body})
}

func WriteErrorWithResource(w standardhttp.ResponseWriter, err error, resource Resource) {
	status, body := publicError(err)
	WriteJSON(w, status, ErrorResponse{Error: body, Resource: &resource})
}

func SetPublicHeaders(w standardhttp.ResponseWriter, image bool) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	robots := "noindex, nofollow, noarchive"
	if image {
		robots += ", noimageindex"
	}
	w.Header().Set("X-Robots-Tag", robots)
}

func MultipartRequestLimit(partLimits ...int64) (int64, error) {
	if len(partLimits) == 0 {
		return 0, invalidRequest("multipart part limits are required", nil)
	}

	total := MultipartOverheadBytes
	for _, limit := range partLimits {
		if limit < 0 || limit > math.MaxInt64-total {
			return 0, invalidRequest("invalid multipart aggregate limit", nil)
		}
		total += limit
	}
	return total, nil
}

func ReadMultipart(
	w standardhttp.ResponseWriter,
	r *standardhttp.Request,
	requestLimit int64,
	partLimit int64,
	allowedFileFields ...string,
) (MultipartForm, error) {
	if requestLimit < 0 || partLimit < 0 || len(allowedFileFields) == 0 {
		return MultipartForm{}, invalidRequest("invalid multipart limits or fields", nil)
	}

	limits := make(map[string]int64, len(allowedFileFields))
	for _, field := range allowedFileFields {
		if field == "" || field == "expires_in_seconds" {
			return MultipartForm{}, invalidRequest("invalid multipart file field", nil)
		}
		limits[field] = partLimit
	}
	return readMultipart(w, r, requestLimit, limits)
}

func ReadMultipartFields(
	w standardhttp.ResponseWriter,
	r *standardhttp.Request,
	requestLimit int64,
	fileLimits ...MultipartFileLimit,
) (MultipartForm, error) {
	if requestLimit < 0 || len(fileLimits) == 0 {
		return MultipartForm{}, invalidRequest("invalid multipart limits or fields", nil)
	}

	limits := make(map[string]int64, len(fileLimits))
	for _, field := range fileLimits {
		if field.FieldName == "" || field.FieldName == "expires_in_seconds" || field.MaxBytes < 0 {
			return MultipartForm{}, invalidRequest("invalid multipart file field", nil)
		}
		if _, exists := limits[field.FieldName]; exists {
			return MultipartForm{}, invalidRequest("duplicate multipart file limit", nil)
		}
		limits[field.FieldName] = field.MaxBytes
	}
	return readMultipart(w, r, requestLimit, limits)
}

func readMultipart(
	w standardhttp.ResponseWriter,
	r *standardhttp.Request,
	requestLimit int64,
	fileLimits map[string]int64,
) (MultipartForm, error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return MultipartForm{}, invalidRequest("invalid multipart form", err)
	}

	boundedBody := standardhttp.MaxBytesReader(w, r.Body, requestLimit)
	r.Body = boundedBody
	reader := multipart.NewReader(boundedBody, params["boundary"])
	form := MultipartForm{Files: make([]MultipartFile, 0)}
	sawPart := false
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			if !sawPart {
				return MultipartForm{}, invalidRequest("invalid multipart form", nil)
			}
			if _, drainErr := io.Copy(io.Discard, boundedBody); drainErr != nil {
				return MultipartForm{}, multipartReadError(drainErr)
			}
			return form, nil
		}
		if nextErr != nil {
			return MultipartForm{}, multipartReadError(nextErr)
		}
		sawPart = true

		fieldName := part.FormName()
		filename := part.FileName()

		if fieldName == "expires_in_seconds" {
			if filename != "" || form.ExpiresInSeconds != nil {
				return MultipartForm{}, invalidRequest("duplicate or invalid expires_in_seconds", nil)
			}
			content, readErr := ReadPart(part, maxExpirationFieldBytes)
			closeErr := part.Close()
			if readErr != nil {
				var maxBytesErr *standardhttp.MaxBytesError
				if errors.As(readErr, &maxBytesErr) {
					return MultipartForm{}, readErr
				}
				if closeErr != nil {
					return MultipartForm{}, multipartReadError(closeErr)
				}
				if common.HasCode(readErr, common.CodeContentTooLarge) {
					return MultipartForm{}, invalidRequest("invalid expires_in_seconds", readErr)
				}
				return MultipartForm{}, readErr
			}
			if closeErr != nil {
				return MultipartForm{}, multipartReadError(closeErr)
			}
			expires, parseErr := strconv.ParseInt(string(content), 10, 64)
			if parseErr != nil {
				return MultipartForm{}, invalidRequest("invalid expires_in_seconds", parseErr)
			}
			form.ExpiresInSeconds = &expires
			continue
		}

		partLimit, ok := fileLimits[fieldName]
		if !ok || filename == "" {
			return MultipartForm{}, invalidRequest("unexpected multipart field", nil)
		}
		content, readErr := ReadPart(part, partLimit)
		closeErr := part.Close()
		if readErr != nil {
			return MultipartForm{}, readErr
		}
		if closeErr != nil {
			return MultipartForm{}, multipartReadError(closeErr)
		}
		form.Files = append(form.Files, MultipartFile{
			FieldName: fieldName,
			Filename:  filename,
			Content:   content,
		})
	}
}

func ReadPart(part *multipart.Part, limit int64) ([]byte, error) {
	if part == nil || limit < 0 {
		return nil, invalidRequest("invalid multipart part", nil)
	}

	content, err := io.ReadAll(&io.LimitedReader{R: part, N: limit})
	if err != nil {
		return nil, multipartReadError(err)
	}

	var overflow [1]byte
	read, err := part.Read(overflow[:])
	if read != 0 {
		return nil, common.NewError(common.CodeContentTooLarge, "content too large", nil)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, multipartReadError(err)
	}
	return content, nil
}

func publicError(err error) (int, ErrorBody) {
	var domainErr *common.Error
	if !errors.As(err, &domainErr) || domainErr == nil {
		return standardhttp.StatusInternalServerError, ErrorBody{
			Code:    common.CodeInternal,
			Message: "internal error",
		}
	}

	status, ok := errorStatuses[domainErr.Code]
	if !ok || domainErr.Code == common.CodeInternal {
		return standardhttp.StatusInternalServerError, ErrorBody{
			Code:    common.CodeInternal,
			Message: "internal error",
		}
	}
	return status, ErrorBody{Code: domainErr.Code, Message: domainErr.Message}
}

var errorStatuses = map[common.ErrorCode]int{
	common.CodeInvalidRequest:       standardhttp.StatusBadRequest,
	common.CodeNotFound:             standardhttp.StatusNotFound,
	common.CodeContentTooLarge:      standardhttp.StatusRequestEntityTooLarge,
	common.CodeUnsupportedMediaType: standardhttp.StatusUnsupportedMediaType,
	common.CodeStorageUnavailable:   standardhttp.StatusServiceUnavailable,
	common.CodeInternal:             standardhttp.StatusInternalServerError,
}

func multipartReadError(err error) error {
	var maxBytesErr *standardhttp.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return common.NewError(common.CodeContentTooLarge, "content too large", err)
	}
	return invalidRequest("invalid multipart form", err)
}

func invalidRequest(message string, err error) error {
	return common.NewError(common.CodeInvalidRequest, message, err)
}
