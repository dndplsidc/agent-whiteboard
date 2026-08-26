package whiteboard

import (
	"context"
	"net/http"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	httpx "github.com/dndplsidc/agent-whiteboard/internal/webapi"
)

type Operations interface {
	CreateMarkdown(context.Context, CreateInput) (Result, error)
	CreateHTML(context.Context, CreateInput) (Result, error)
	Get(context.Context, string) (Whiteboard, error)
	Update(context.Context, UpdateInput) (Result, error)
	Delete(context.Context, Kind, string) error
}

type HandlerConfig struct {
	MaxWhiteboardBytes int64
	MaxContextBytes    int64
}

type Handler struct {
	operations                Operations
	viewer                    *Viewer
	maxWhiteboardBytes        int64
	maxContextBytes           int64
	maxWhiteboardRequestBytes int64
}

func NewHandler(operations Operations, viewer *Viewer, config HandlerConfig) (*Handler, error) {
	switch {
	case common.IsNil(operations):
		return nil, common.NewError(common.CodeInvalidRequest, "operations are required", nil)
	case viewer == nil:
		return nil, common.NewError(common.CodeInvalidRequest, "viewer is required", nil)
	case config.MaxWhiteboardBytes < 0:
		return nil, common.NewError(common.CodeInvalidRequest, "max whiteboard bytes must not be negative", nil)
	case config.MaxContextBytes < 0:
		return nil, common.NewError(common.CodeInvalidRequest, "max context bytes must not be negative", nil)
	}

	maxWhiteboardRequestBytes, err := httpx.MultipartRequestLimit(config.MaxWhiteboardBytes, config.MaxContextBytes)
	if err != nil {
		return nil, err
	}
	return &Handler{
		operations:                operations,
		viewer:                    viewer,
		maxWhiteboardBytes:        config.MaxWhiteboardBytes,
		maxContextBytes:           config.MaxContextBytes,
		maxWhiteboardRequestBytes: maxWhiteboardRequestBytes,
	}, nil
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+httpx.APIWhiteboardMarkdown, h.createMarkdown)
	mux.HandleFunc("GET "+httpx.APIWhiteboardMarkdownResource+"{id}", h.getMarkdown)
	mux.HandleFunc("PUT "+httpx.APIWhiteboardMarkdownResource+"{id}", h.updateMarkdown)
	mux.HandleFunc("DELETE "+httpx.APIWhiteboardMarkdown+"/{id}", h.deleteMarkdown)
	mux.HandleFunc("POST "+httpx.APIWhiteboardHTML, h.createHTML)
	mux.HandleFunc("GET "+httpx.APIWhiteboardHTMLResource+"{id}", h.getHTML)
	mux.HandleFunc("PUT "+httpx.APIWhiteboardHTML+"/{id}", h.updateHTML)
	mux.HandleFunc("DELETE "+httpx.APIWhiteboardHTML+"/{id}", h.deleteHTML)
	mux.HandleFunc("GET "+httpx.PublicMarkdown+"{id}", h.viewMarkdown)
	mux.HandleFunc("GET "+httpx.PublicHTML+"{id}", h.viewHTML)
	mux.HandleFunc("GET "+httpx.PublicHTML+"{id}"+httpx.PublicHTMLContentSuffix, h.viewHTMLContent)
	mux.HandleFunc("GET "+httpx.PublicHTML+"{id}"+httpx.PublicHTMLRenderedSuffix, h.viewHTMLRendered)
}

func (h *Handler) getMarkdown(w http.ResponseWriter, r *http.Request) {
	board, ok := h.loadMachineWhiteboard(w, r, KindMarkdown)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.MarkdownResponse{
		Resource: resourceFromWhiteboard(board),
		Markdown: string(board.Source),
		Context:  string(board.Context),
	})
}

func (h *Handler) getHTML(w http.ResponseWriter, r *http.Request) {
	board, ok := h.loadMachineWhiteboard(w, r, KindHTML)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.HTMLResponse{
		Resource: resourceFromWhiteboard(board),
		HTML:     string(board.Source),
		Context:  string(board.Context),
	})
}

func (h *Handler) loadMachineWhiteboard(w http.ResponseWriter, r *http.Request, kind Kind) (Whiteboard, bool) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("id")
	if err := common.ValidateID(id); err != nil {
		httpx.WriteError(w, notFound())
		return Whiteboard{}, false
	}
	board, err := h.operations.Get(r.Context(), id)
	if err != nil {
		if common.HasCode(err, common.CodeNotFound) {
			err = notFound()
		}
		httpx.WriteError(w, err)
		return Whiteboard{}, false
	}
	if board.Kind != kind {
		httpx.WriteError(w, notFound())
		return Whiteboard{}, false
	}
	return board, true
}

func (h *Handler) viewMarkdown(w http.ResponseWriter, r *http.Request) {
	h.setMarkdownHeaders(w)
	board, ok := h.loadPublicWhiteboard(w, r, KindMarkdown)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_ = h.viewer.Render(w, board)
	}
}

func (h *Handler) viewHTML(w http.ResponseWriter, r *http.Request) {
	h.setHTMLHeaders(w)
	board, ok := h.loadPublicWhiteboard(w, r, KindHTML)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_ = h.viewer.Render(w, board)
	}
}

func (h *Handler) viewHTMLContent(w http.ResponseWriter, r *http.Request) {
	setStandaloneInnerHeaders(w)
	board, ok := h.loadPublicWhiteboard(w, r, KindHTML)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(board.Source)
	}
}

func (h *Handler) viewHTMLRendered(w http.ResponseWriter, r *http.Request) {
	setStandaloneInnerHeaders(w)
	board, ok := h.loadPublicWhiteboard(w, r, KindHTML)
	if !ok {
		return
	}
	rendered, err := h.viewer.renderHTMLChild(board.Source)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(rendered)
	}
}

func (h *Handler) loadPublicWhiteboard(w http.ResponseWriter, r *http.Request, kind Kind) (Whiteboard, bool) {
	id := r.PathValue("id")
	if err := common.ValidateID(id); err != nil {
		httpx.WriteError(w, notFound())
		return Whiteboard{}, false
	}

	board, err := h.operations.Get(r.Context(), id)
	if err != nil {
		if common.HasCode(err, common.CodeNotFound) {
			err = notFound()
		}
		httpx.WriteError(w, err)
		return Whiteboard{}, false
	}
	if board.Kind != kind {
		httpx.WriteError(w, notFound())
		return Whiteboard{}, false
	}
	return board, true
}

func (h *Handler) setMarkdownHeaders(w http.ResponseWriter) {
	setPresentationHeaders(w)
	w.Header().Set("Content-Security-Policy", h.viewer.ContentSecurityPolicy())
	w.Header().Set("X-Frame-Options", "DENY")
}

func (h *Handler) setHTMLHeaders(w http.ResponseWriter) {
	setPresentationHeaders(w)
	w.Header().Set("Content-Security-Policy", h.viewer.HTMLContentSecurityPolicy())
	w.Header().Set("X-Frame-Options", "DENY")
}

func setStandaloneInnerHeaders(w http.ResponseWriter) {
	setPresentationHeaders(w)
	w.Header().Set("Content-Security-Policy", StandaloneInnerContentSecurityPolicy)
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
}

func setPresentationHeaders(w http.ResponseWriter) {
	httpx.SetPublicHeaders(w, false)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", RestrictivePermissionsPolicy)
}

func (h *Handler) createMarkdown(w http.ResponseWriter, r *http.Request) {
	h.create(w, r, KindMarkdown)
}

func (h *Handler) createHTML(w http.ResponseWriter, r *http.Request) {
	h.create(w, r, KindHTML)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, kind Kind) {
	input, err := h.readCreateInput(w, r, kind)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}

	var result Result
	if kind == KindMarkdown {
		result, err = h.operations.CreateMarkdown(r.Context(), input)
	} else {
		result, err = h.operations.CreateHTML(r.Context(), input)
	}
	if err != nil {
		if result.ID != "" {
			httpx.WriteErrorWithResource(w, err, resourceFromResult(result, kind))
			return
		}
		httpx.WriteError(w, err)
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, httpx.ResourceResponse{
		Resource: resourceFromResult(result, kind),
	})
}

func (h *Handler) updateMarkdown(w http.ResponseWriter, r *http.Request) {
	h.update(w, r, KindMarkdown)
}

func (h *Handler) updateHTML(w http.ResponseWriter, r *http.Request) {
	h.update(w, r, KindHTML)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request, kind Kind) {
	id := r.PathValue("id")
	if err := common.ValidateID(id); err != nil {
		httpx.WriteError(w, notFound())
		return
	}

	input, err := h.readUpdateInput(w, r, id, kind)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	result, err := h.operations.Update(r.Context(), input)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, httpx.ResourceResponse{
		Resource: resourceFromResult(result, kind),
	})
}

func (h *Handler) deleteMarkdown(w http.ResponseWriter, r *http.Request) {
	h.delete(w, r, KindMarkdown)
}

func (h *Handler) deleteHTML(w http.ResponseWriter, r *http.Request) {
	h.delete(w, r, KindHTML)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request, kind Kind) {
	id := r.PathValue("id")
	if err := common.ValidateID(id); err != nil {
		httpx.WriteError(w, notFound())
		return
	}
	if err := h.operations.Delete(r.Context(), kind, id); err != nil {
		httpx.WriteError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) readCreateInput(w http.ResponseWriter, r *http.Request, _ Kind) (CreateInput, error) {
	source, creatorContext, expiresInSeconds, err := h.readWhiteboardPair(w, r)
	if err != nil {
		return CreateInput{}, err
	}
	return CreateInput{Source: source, Context: creatorContext, ExpiresInSeconds: expiresInSeconds}, nil
}

func (h *Handler) readUpdateInput(w http.ResponseWriter, r *http.Request, id string, kind Kind) (UpdateInput, error) {
	source, creatorContext, expiresInSeconds, err := h.readWhiteboardPair(w, r)
	if err != nil {
		return UpdateInput{}, err
	}
	return UpdateInput{
		ID: id, Kind: kind, Source: source, Context: creatorContext, ExpiresInSeconds: expiresInSeconds,
	}, nil
}

func (h *Handler) readWhiteboardPair(w http.ResponseWriter, r *http.Request) ([]byte, []byte, *int64, error) {
	form, err := httpx.ReadMultipartFields(w, r, h.maxWhiteboardRequestBytes,
		httpx.MultipartFileLimit{FieldName: "file", MaxBytes: h.maxWhiteboardBytes},
		httpx.MultipartFileLimit{FieldName: "context", MaxBytes: h.maxContextBytes},
	)
	if err != nil {
		return nil, nil, nil, err
	}

	var source, creatorContext []byte
	fileCount, contextCount := 0, 0
	for _, file := range form.Files {
		switch file.FieldName {
		case "file":
			fileCount++
			source = file.Content
		case "context":
			contextCount++
			creatorContext = file.Content
		}
	}
	if fileCount != 1 || contextCount != 1 {
		return nil, nil, nil, common.NewError(common.CodeInvalidRequest, "exactly one file and context are required", nil)
	}
	if len(source) == 0 || len(creatorContext) == 0 {
		return nil, nil, nil, common.NewError(common.CodeInvalidRequest, "file and context must not be empty", nil)
	}
	return source, creatorContext, form.ExpiresInSeconds, nil
}

func resourceFromWhiteboard(board Whiteboard) httpx.Resource {
	return resourceFromResult(Result{
		ID:        board.ID,
		Kind:      board.Kind,
		CreatedAt: board.CreatedAt,
		UpdatedAt: board.UpdatedAt,
		ExpiresAt: board.ExpiresAt,
	}, board.Kind)
}

func resourceFromResult(result Result, kind Kind) httpx.Resource {
	resource := httpx.Resource{
		ID:        result.ID,
		Type:      string(kind),
		Path:      publicPath(kind) + result.ID,
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

func publicPath(kind Kind) string {
	if kind == KindMarkdown {
		return httpx.PublicMarkdown
	}
	return httpx.PublicHTML
}
