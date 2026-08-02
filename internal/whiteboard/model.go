package whiteboard

import "time"

type Kind string

const (
	KindMarkdown Kind = "markdown"
	KindHTML     Kind = "html"
)

type Whiteboard struct {
	ID        string
	Kind      Kind
	Source    []byte
	Context   []byte
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt *time.Time
}

type CreateInput struct {
	Source           []byte
	Context          []byte
	ExpiresInSeconds *int64
}

type UpdateInput struct {
	ID               string
	Kind             Kind
	Source           []byte
	Context          []byte
	ExpiresInSeconds *int64
}

type Result struct {
	ID        string
	Kind      Kind
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt *time.Time
}
