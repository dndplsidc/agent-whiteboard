package image

import (
	"context"
	"mime"
	"net/http"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	httpx "github.com/dndplsidc/agent-whiteboard/internal/webapi"
)

type Operations interface {
	CreateImages(context.Context, CreateInput) ([]Result, error)
	Get(context.Context, string) (Image, error)
	Update(context.Context, UpdateInput) (Result, error)
	Delete(context.Context, string) error
}

type HandlerConfig struct {
	MaxImageBytes   int64
	MaxRequestBytes int64
}

type Handler struct {
	operations      Operations
	maxImageBytes   int64
	maxRequestBytes int64
}

func NewHandler(operations Operations, config HandlerConfig) (*Handler, error) {
	switch {
	case common.IsNil(operations):
		return nil, common.NewError(common.CodeInvalidRequest, "operations are required", nil)
	case config.MaxImageBytes < 0:
		return nil, common.NewError(common.CodeInvalidRequest, "max image bytes must not be negative", nil)
	case config.MaxRequestBytes < 0:
		return nil, common.NewError(common.CodeInvalidRequest, "max request bytes must not be negative", nil)
	case config.MaxRequestBytes < config.MaxImageBytes:
		return nil, common.NewError(common.CodeInvalidRequest, "max request bytes must not be less than max image bytes", nil)
	}

	return &Handler{
		operations:      operations,
		maxImageBytes:   config.MaxImageBytes,
		maxRequestBytes: config.MaxRequestBytes,
	}, nil
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+httpx.APIImages, h.create)
	mux.HandleFunc("PUT "+httpx.APIImages+"/{id}", h.update)
	mux.HandleFunc("DELETE "+httpx.APIImages+"/{id}", h.delete)
	mux.HandleFunc("GET "+httpx.PublicImages+"{id}", h.view)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	form, err := httpx.ReadMultipart(w, r, h.maxRequestBytes, h.maxImageBytes, "images")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if len(form.Files) == 0 {
		httpx.WriteError(w, common.NewError(common.CodeInvalidRequest, "at least one image is required", nil))
		return
	}

	input := CreateInput{Images: make([]Upload, len(form.Files))}
	for index, file := range form.Files {
		input.Images[index] = Upload{
			Content:          file.Content,
			ExpiresInSeconds: form.ExpiresInSeconds,
		}
	}
	results, err := h.operations.CreateImages(r.Context(), input)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}

	resources := make([]httpx.Resource, len(results))
	for index, result := range results {
		resources[index] = imageResourceFromResult(result)
	}
	httpx.WriteJSON(w, http.StatusCreated, httpx.ImagesResponse{Images: resources})
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := common.ValidateID(id); err != nil {
		httpx.WriteError(w, notFound())
		return
	}

	form, err := httpx.ReadMultipart(w, r, h.maxRequestBytes, h.maxImageBytes, "file")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if len(form.Files) != 1 {
		httpx.WriteError(w, common.NewError(common.CodeInvalidRequest, "exactly one file is required", nil))
		return
	}

	result, err := h.operations.Update(r.Context(), UpdateInput{
		ID:               id,
		Content:          form.Files[0].Content,
		ExpiresInSeconds: form.ExpiresInSeconds,
	})
	if err != nil {
		httpx.WriteError(w, err)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, httpx.ResourceResponse{
		Resource: imageResourceFromResult(result),
	})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := common.ValidateID(id); err != nil {
		httpx.WriteError(w, notFound())
		return
	}
	if err := h.operations.Delete(r.Context(), id); err != nil {
		httpx.WriteError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) view(w http.ResponseWriter, r *http.Request) {
	httpx.SetPublicHeaders(w, true)
	id := r.PathValue("id")
	if err := common.ValidateID(id); err != nil {
		httpx.WriteError(w, notFound())
		return
	}

	record, err := h.operations.Get(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}

	w.Header().Set("Content-Type", record.MediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{
		"filename": record.ID + record.Extension,
	}))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(record.Content)
}

func imageResourceFromResult(result Result) httpx.Resource {
	resource := httpx.Resource{
		ID:        result.ID,
		Filename:  result.ID + result.Extension,
		Extension: result.Extension,
		MediaType: result.MediaType,
		Path:      httpx.PublicImages + result.ID,
		CreatedAt: result.CreatedAt,
		UpdatedAt: result.UpdatedAt,
		Permanent: result.ExpiresAt == nil,
	}
	if result.ExpiresAt != nil {
		expiresAt := result.ExpiresAt.Unix()
		resource.ExpiresAt = &expiresAt
	}
	return resource
}
