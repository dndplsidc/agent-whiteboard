package whiteboard

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"golang.org/x/net/html"
)

func validateHTML(source []byte) error {
	if !utf8.Valid(source) {
		return common.NewError(common.CodeInvalidRequest, "html must be UTF-8", nil)
	}
	var hasDoctype, hasHTML, hasHead, hasBody bool
	tokenizer := html.NewTokenizer(bytes.NewReader(source))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			err := tokenizer.Err()
			if err != nil && err != io.EOF {
				return common.NewError(common.CodeInvalidRequest, "html is invalid", err)
			}
			if err := requireDocumentTokens(hasDoctype, hasHTML, hasHead, hasBody); err != nil {
				return err
			}
			if _, err := htmlHeadStartOffset(source); err != nil {
				return common.NewError(common.CodeInvalidRequest, "html must begin its head before publisher content", err)
			}
			return validateExplicitDeclarations(source)
		case html.DoctypeToken:
			hasDoctype = true
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			switch {
			case strings.EqualFold(token.Data, "html"):
				hasHTML = true
			case strings.EqualFold(token.Data, "head"):
				hasHead = true
			case strings.EqualFold(token.Data, "body"):
				hasBody = true
			case strings.EqualFold(token.Data, "script") && hasAttribute(token, "src"):
				return common.NewError(common.CodeInvalidRequest, "html must not include scripts with src", nil)
			case strings.EqualFold(token.Data, "link") && attributeHasToken(token, "rel", "stylesheet"):
				return common.NewError(common.CodeInvalidRequest, "html must not include stylesheet links", nil)
			}
		}
	}
}

var errPublisherContentBeforeHead = errors.New("publisher content precedes html head")

var supportedExplicitTypes = map[string]struct{}{
	"section": {}, "image": {}, "chart": {}, "table": {}, "code": {}, "quote": {}, "component": {},
}

func validateExplicitDeclarations(source []byte) error {
	document, err := html.Parse(bytes.NewReader(source))
	if err != nil {
		return common.NewError(common.CodeInvalidRequest, "html is invalid", err)
	}

	ids := make(map[string][]*html.Node)
	var elements []*html.Node
	walkElements(document, func(node *html.Node) {
		elements = append(elements, node)
		if id, ok := attribute(node, "id"); ok && id != "" {
			ids[id] = append(ids[id], node)
		}
	})

	for _, node := range elements {
		selectValue, hasSelect := attribute(node, "data-agent-select")
		hasSection := hasNodeAttribute(node, "data-agent-section")
		hasIgnore := hasNodeAttribute(node, "data-agent-section-ignore")
		if !hasSelect && !hasSection {
			continue
		}
		if hasSelect && selectValue != "none" {
			if _, ok := supportedExplicitTypes[selectValue]; !ok {
				return invalidExplicitDeclaration("html has an unsupported explicit selection value")
			}
		}
		if hasSelect && (selectValue == "" || (selectValue == "none" && hasSection)) {
			return invalidExplicitDeclaration("html has a conflicting component declaration")
		}
		if hasSelect && hasSection {
			return invalidExplicitDeclaration("html has a conflicting component declaration")
		}
		if hasIgnore && hasSection {
			return invalidExplicitDeclaration("html has a conflicting component declaration")
		}
		if hasIgnore && hasSelect && selectValue != "none" {
			return invalidExplicitDeclaration("html has a conflicting component declaration")
		}
		if hasSelect && selectValue == "none" {
			continue
		}

		id, ok := attribute(node, "id")
		if !ok || !boundedSourceText(id, 256) || strings.TrimSpace(id) != id {
			return invalidExplicitDeclaration("html explicit component has an invalid id")
		}
		if len(ids[id]) != 1 {
			return invalidExplicitDeclaration("html explicit component id is not unique")
		}
		kind := selectValue
		if hasSection {
			kind = "section"
		}
		if explicitExcluded(node, kind) {
			return invalidExplicitDeclaration("html explicit component is in excluded content")
		}
		if explicitLabel(node, kind, ids) == "" {
			return invalidExplicitDeclaration("html explicit component is missing a source-resolvable label")
		}
	}
	return nil
}

func walkElements(node *html.Node, visit func(*html.Node)) {
	if node.Type == html.ElementNode {
		visit(node)
		if strings.EqualFold(node.Data, "template") {
			return
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		walkElements(child, visit)
	}
}

func attribute(node *html.Node, name string) (string, bool) {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, name) {
			return attr.Val, true
		}
	}
	return "", false
}

func hasNodeAttribute(node *html.Node, name string) bool {
	_, ok := attribute(node, name)
	return ok
}

func hasAttributeValue(node *html.Node, name, value string) bool {
	got, ok := attribute(node, name)
	return ok && strings.EqualFold(got, value)
}

func explicitLabel(node *html.Node, kind string, ids map[string][]*html.Node) string {
	if references, ok := attribute(node, "aria-labelledby"); ok {
		var parts []string
		for _, id := range strings.Fields(references) {
			matches := ids[id]
			if len(matches) != 1 {
				parts = nil
				break
			}
			if text := nodeText(matches[0]); text != "" {
				parts = append(parts, text)
			} else {
				parts = nil
				break
			}
		}
		if label := validSourceLabel(strings.Join(parts, " ")); label != "" {
			return label
		}
	}
	if label, ok := attribute(node, "aria-label"); ok {
		if label = validSourceLabel(label); label != "" {
			return label
		}
	}
	var fallback string
	switch kind {
	case "image", "chart":
		if strings.EqualFold(node.Data, "figure") {
			fallback = directChildText(node, "figcaption")
			if fallback == "" {
				fallback = firstDescendantAttribute(node, "img", "alt")
			}
		} else {
			if label, ok := attribute(node, "alt"); ok {
				fallback = label
			} else {
				fallback = firstDescendantAttribute(node, "img", "alt")
			}
			if fallback == "" {
				fallback = firstDescendantText(node, func(name string) bool { return name == "title" })
			}
		}
	case "section":
		fallback = firstDescendantText(node, func(name string) bool {
			return len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6'
		})
	case "table":
		fallback = directChildText(node, "caption")
	case "code":
		fallback = codeFallbackLabel(node)
	case "quote":
		fallback = previewLabel(nodeText(node))
	}
	return validSourceLabel(fallback)
}

var hiddenStylePattern = regexp.MustCompile(`(?i)(?:^|;)\s*(?:display\s*:\s*none|visibility\s*:\s*hidden)\s*(?:;|$)`)

func boundedSourceText(value string, limit int) bool {
	if value == "" || len([]byte(value)) > limit {
		return false
	}
	for _, character := range value {
		if character < 0x20 && character != '\t' && character != '\n' && character != '\r' || character == 0x7f {
			return false
		}
	}
	return true
}

func validSourceLabel(value string) string {
	value = normalizedText(value)
	if !boundedSourceText(value, 256) {
		return ""
	}
	return value
}

func explicitExcluded(node *html.Node, _ string) bool {
	for current := node; current != nil; current = current.Parent {
		if current != node && (hasNodeAttribute(current, "data-agent-section-ignore") || hasAttributeValue(current, "data-agent-select", "none")) {
			return true
		}
		name := strings.ToLower(current.Data)
		if hasNodeAttribute(current, "hidden") || hasNodeAttribute(current, "inert") {
			return true
		}
		if value, ok := attribute(current, "aria-hidden"); ok && strings.EqualFold(strings.TrimSpace(value), "true") {
			return true
		}
		if style, ok := attribute(current, "style"); ok && hiddenStylePattern.MatchString(style) {
			return true
		}
		switch name {
		case "nav", "header", "footer", "form", "input", "button", "select", "textarea", "option", "fieldset", "script", "style", "template", "noscript", "video", "audio":
			// Descendant controls are deliberately not traversed, so an ordinary
			// custom wrapper may contain them without making the wrapper ineligible.
			return true
		}
	}
	return false
}

func firstDescendantAttribute(node *html.Node, element, name string) string {
	var find func(*html.Node) (string, bool)
	find = func(current *html.Node) (string, bool) {
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.ElementNode && strings.EqualFold(child.Data, "template") {
				continue
			}
			if child.Type == html.ElementNode && strings.EqualFold(child.Data, element) {
				value, _ := attribute(child, name)
				return normalizedText(value), true
			}
			if value, found := find(child); found {
				return value, true
			}
		}
		return "", false
	}
	value, _ := find(node)
	return value
}

func codeFallbackLabel(node *html.Node) string {
	code := node
	if !strings.EqualFold(code.Data, "code") {
		if descendant := firstDescendantElement(node, "code"); descendant != nil {
			code = descendant
		}
	}
	if language, ok := attribute(node, "data-language"); ok && normalizedText(language) != "" {
		return normalizedText(language)
	}
	if language, ok := attribute(code, "data-language"); ok && normalizedText(language) != "" {
		return normalizedText(language)
	}
	if className, ok := attribute(code, "class"); ok {
		for _, class := range strings.Fields(className) {
			if strings.HasPrefix(class, "language-") && len(class) > len("language-") {
				return strings.TrimPrefix(class, "language-") + " code"
			}
		}
	}
	return previewLabel(nodeText(node))
}

func previewLabel(value string) string {
	characters := []rune(normalizedText(value))
	if len(characters) > 160 {
		characters = characters[:160]
	}
	return string(characters)
}

func firstDescendantElement(node *html.Node, name string) *html.Node {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && strings.EqualFold(child.Data, "template") {
			continue
		}
		if child.Type == html.ElementNode && strings.EqualFold(child.Data, name) {
			return child
		}
		if descendant := firstDescendantElement(child, name); descendant != nil {
			return descendant
		}
	}
	return nil
}

func directChildText(node *html.Node, name string) string {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && strings.EqualFold(child.Data, name) {
			return nodeText(child)
		}
	}
	return ""
}

func firstDescendantText(node *html.Node, match func(string) bool) string {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && strings.EqualFold(child.Data, "template") {
			continue
		}
		if child.Type == html.ElementNode && match(strings.ToLower(child.Data)) {
			if text := nodeText(child); text != "" {
				return text
			}
		}
		if text := firstDescendantText(child, match); text != "" {
			return text
		}
	}
	return ""
}

func nodeText(node *html.Node) string {
	var text strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.ElementNode && strings.EqualFold(current.Data, "template") {
			return
		}
		if current.Type == html.TextNode {
			text.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return normalizedText(text.String())
}

func normalizedText(value string) string { return strings.Join(strings.Fields(value), " ") }

func htmlHeadStartOffset(source []byte) (int, error) {
	tokenizer := html.NewTokenizer(bytes.NewReader(source))
	offset := 0
	bomAllowed := true
	bomSeen := false
	for {
		tokenType := tokenizer.Next()
		raw := tokenizer.Raw()
		offset += len(raw)
		switch tokenType {
		case html.DoctypeToken:
			bomAllowed = false
			continue
		case html.CommentToken:
			if bytes.HasPrefix(raw, []byte("<!--")) && bytes.HasSuffix(raw, []byte("-->")) {
				continue
			}
		case html.TextToken:
			text := string(raw)
			if bomAllowed && !bomSeen {
				trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
				if strings.HasPrefix(trimmed, "\ufeff") {
					bomSeen = true
					index := strings.Index(text, "\ufeff")
					text = text[:index] + text[index+len("\ufeff"):]
				}
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
		case html.StartTagToken:
			token := tokenizer.Token()
			if strings.EqualFold(token.Data, "html") {
				bomAllowed = false
				continue
			}
			if strings.EqualFold(token.Data, "head") {
				return offset, nil
			}
		case html.ErrorToken:
			if err := tokenizer.Err(); err != nil && err != io.EOF {
				return 0, err
			}
			return 0, io.ErrUnexpectedEOF
		}
		return 0, errPublisherContentBeforeHead
	}
}

func invalidExplicitDeclaration(message string) error {
	return common.NewError(common.CodeInvalidRequest, message, nil)
}

func requireDocumentTokens(hasDoctype, hasHTML, hasHead, hasBody bool) error {
	switch {
	case !hasDoctype:
		return common.NewError(common.CodeInvalidRequest, "html must include a doctype", nil)
	case !hasHTML:
		return common.NewError(common.CodeInvalidRequest, "html must include an html element", nil)
	case !hasHead:
		return common.NewError(common.CodeInvalidRequest, "html must include a head element", nil)
	case !hasBody:
		return common.NewError(common.CodeInvalidRequest, "html must include a body element", nil)
	default:
		return nil
	}
}

func hasAttribute(token html.Token, name string) bool {
	for _, attribute := range token.Attr {
		if strings.EqualFold(attribute.Key, name) {
			return true
		}
	}
	return false
}

func attributeHasToken(token html.Token, name, want string) bool {
	for _, attribute := range token.Attr {
		if !strings.EqualFold(attribute.Key, name) {
			continue
		}
		for _, value := range strings.Fields(attribute.Val) {
			if strings.EqualFold(value, want) {
				return true
			}
		}
	}
	return false
}
