package whiteboard

import (
	"bytes"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/edocsss/agent-whiteboard/internal/common"
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
		if hasIgnore && hasSelect && selectValue != "none" {
			return invalidExplicitDeclaration("html has a conflicting component declaration")
		}
		if hasSelect && selectValue == "none" {
			continue
		}

		id, ok := attribute(node, "id")
		if !ok || strings.TrimSpace(id) == "" {
			return invalidExplicitDeclaration("html explicit component is missing an id")
		}
		if len(ids[id]) != 1 {
			return invalidExplicitDeclaration("html explicit component id is not unique")
		}
		kind := selectValue
		if hasSection {
			kind = "section"
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

func explicitLabel(node *html.Node, kind string, ids map[string][]*html.Node) string {
	if label, ok := attribute(node, "aria-label"); ok {
		return normalizedText(label)
	}
	if references, ok := attribute(node, "aria-labelledby"); ok {
		var parts []string
		for _, id := range strings.Fields(references) {
			matches := ids[id]
			if len(matches) != 1 {
				return ""
			}
			if text := nodeText(matches[0]); text != "" {
				parts = append(parts, text)
			}
		}
		return normalizedText(strings.Join(parts, " "))
	}
	switch kind {
	case "image":
		if label, ok := attribute(node, "alt"); ok {
			return normalizedText(label)
		}
	case "section":
		return firstDescendantText(node, func(name string) bool {
			return len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6'
		})
	case "table":
		return firstDescendantText(node, func(name string) bool { return name == "caption" })
	case "code", "quote":
		return nodeText(node)
	}
	return ""
}

func firstDescendantText(node *html.Node, match func(string) bool) string {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
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
