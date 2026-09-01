package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

func listPage(next string, sessions ...any) scriptedListPage {
	if sessions == nil {
		sessions = []any{}
	}
	result := map[string]any{"sessions": sessions}
	if next != "" {
		result["nextCursor"] = next
	}
	return scriptedListPage{result: result}
}

func listedSession(id string, options any) map[string]any {
	item := map[string]any{"sessionId": id}
	if options != nil {
		item["configOptions"] = options
	}
	return item
}

func inspectRef(t *testing.T, driver *Driver, id string) (provider.NativeSession, error) {
	t.Helper()
	ref, err := provider.NewNativeSessionRef(id)
	if err != nil {
		t.Fatal(err)
	}
	return driver.Inspect(context.Background(), provider.InspectRequest{Provider: provider.NameCursor, NativeSession: ref})
}

func requireProviderCode(t *testing.T, err error, code provider.ProviderErrorCode) {
	t.Helper()
	var got provider.ProviderError
	if !errors.As(err, &got) || got.Code() != code {
		t.Fatalf("error = %v, want provider code %v", err, code)
	}
}

func configurePages(launcher *scriptLauncher, pages map[string]scriptedListPage) {
	launcher.mu.Lock()
	launcher.listPages = pages
	launcher.mu.Unlock()
}

func TestInspectTraversesAllPagesAndUsesExactContinuationParams(t *testing.T) {
	for _, target := range []string{"first", "middle", "final"} {
		t.Run(target, func(t *testing.T) {
			driver, launcher, _ := testDriver(t)
			configurePages(launcher, map[string]scriptedListPage{
				"":    listPage("one", listedSession("first", testOptions("model-a"))),
				"one": listPage("two", listedSession("middle", testOptions("model-b"))),
				"two": listPage("", listedSession("final", testOptions("model-a"))),
			})
			got, err := inspectRef(t, driver, target)
			if err != nil || got.Ref.Value() != target {
				t.Fatalf("Inspect = %+v, %v", got, err)
			}
			launcher.mu.Lock()
			params := append([]json.RawMessage(nil), launcher.children[0].listParams...)
			launcher.mu.Unlock()
			want := []string{"{}", `{"cursor":"one"}`, `{"cursor":"two"}`}
			if len(params) != len(want) {
				t.Fatalf("params = %q", params)
			}
			for i := range want {
				if string(params[i]) != want[i] {
					t.Fatalf("params[%d] = %s, want %s", i, params[i], want[i])
				}
			}
		})
	}
}

func TestInspectAcceptsEmptyTerminalPageAndReportsTerminalAbsence(t *testing.T) {
	driver, launcher, _ := testDriver(t)
	configurePages(launcher, map[string]scriptedListPage{
		"":      listPage("empty", listedSession("other", nil)),
		"empty": listPage(""),
	})
	_, err := inspectRef(t, driver, "missing")
	requireProviderCode(t, err, provider.ErrorNativeSessionMissing)
}

func TestListTraversalRejectsCursorCyclesAndDuplicates(t *testing.T) {
	tests := map[string]map[string]scriptedListPage{
		"self cursor": {
			"": listPage("same"), "same": listPage("same"),
		},
		"repeated cursor": {
			"": listPage("a"), "a": listPage("b"), "b": listPage("a"),
		},
		"duplicate session across pages": {
			"": listPage("a", listedSession("duplicate", nil)), "a": listPage("", listedSession("duplicate", nil)),
		},
	}
	for name, pages := range tests {
		t.Run(name, func(t *testing.T) {
			driver, launcher, _ := testDriver(t)
			configurePages(launcher, pages)
			_, err := inspectRef(t, driver, "missing")
			requireProviderCode(t, err, provider.ErrorProtocolIncompatible)
		})
	}
}

func TestListTraversalRejectsMalformedPagesIncludingAfterTarget(t *testing.T) {
	badOptions := testOptions("model-a")
	badOptions[0].CurrentValue = "unknown"
	tests := map[string]map[string]scriptedListPage{
		"nil sessions": {
			"": {result: map[string]any{"sessions": nil}},
		},
		"empty present configuration": {
			"": listPage("", listedSession("target", []configOption{})),
		},
		"later malformed configuration": {
			"":      listPage("later", listedSession("target", testOptions("model-a"))),
			"later": listPage("", listedSession("other", badOptions)),
		},
		"present empty cursor": {
			"": {result: map[string]any{"sessions": []any{}, "nextCursor": ""}},
		},
		"control cursor": {
			"": listPage("bad\nvalue"),
		},
		"wrong cursor type": {
			"": {result: map[string]any{"sessions": []any{}, "nextCursor": 7}},
		},
		"oversized cursor": {
			"": listPage(strings.Repeat("c", maxListCursorBytes+1)),
		},
	}
	for name, pages := range tests {
		t.Run(name, func(t *testing.T) {
			driver, launcher, _ := testDriver(t)
			configurePages(launcher, pages)
			_, err := inspectRef(t, driver, "target")
			requireProviderCode(t, err, provider.ErrorProtocolIncompatible)
		})
	}
}

func TestListTraversalHardPageAndSessionBounds(t *testing.T) {
	t.Run("65 pages", func(t *testing.T) {
		driver, launcher, _ := testDriver(t)
		pages := make(map[string]scriptedListPage)
		cursor := ""
		for i := 1; i <= maxListPages; i++ {
			next := fmt.Sprintf("page-%d", i)
			pages[cursor] = listPage(next)
			cursor = next
		}
		configurePages(launcher, pages)
		_, err := inspectRef(t, driver, "missing")
		requireProviderCode(t, err, provider.ErrorProtocolIncompatible)
	})

	t.Run("1001 sessions on page", func(t *testing.T) {
		driver, launcher, _ := testDriver(t)
		sessions := make([]any, maxListSessionsPage+1)
		for i := range sessions {
			sessions[i] = listedSession(fmt.Sprintf("session-%d", i), nil)
		}
		configurePages(launcher, map[string]scriptedListPage{"": listPage("", sessions...)})
		_, err := inspectRef(t, driver, "missing")
		requireProviderCode(t, err, provider.ErrorProtocolIncompatible)
	})

	t.Run("4097 sessions total", func(t *testing.T) {
		driver, launcher, _ := testDriver(t)
		pages := make(map[string]scriptedListPage)
		cursor, id := "", 0
		for page := 0; page < 5; page++ {
			count := 1000
			if page == 4 {
				count = 97
			}
			sessions := make([]any, count)
			for i := range sessions {
				sessions[i] = listedSession(fmt.Sprintf("session-%d", id), nil)
				id++
			}
			next := ""
			if page != 4 {
				next = fmt.Sprintf("cursor-%d", page)
			}
			pages[cursor] = listPage(next, sessions...)
			cursor = next
		}
		configurePages(launcher, pages)
		_, err := inspectRef(t, driver, "missing")
		requireProviderCode(t, err, provider.ErrorProtocolIncompatible)
	})
}

func TestListTraversalRejectsAggregateSemanticBytes(t *testing.T) {
	driver, launcher, _ := testDriver(t)
	values := make([]configValue, provider.MaxCatalogModels)
	for i := range values {
		values[i] = configValue{Value: fmt.Sprintf("v-%d", i), Name: "N", Description: strings.Repeat("d", 1900)}
	}
	options := []configOption{{ID: "model", Name: "Model", Category: "model", Type: "select", CurrentValue: values[0].Value, Options: values}}
	pages := make(map[string]scriptedListPage)
	cursor, id := "", 0
	for page := 0; page < 3; page++ {
		sessions := make([]any, 50)
		for i := range sessions {
			sessions[i] = listedSession(fmt.Sprintf("large-%d", id), options)
			id++
		}
		next := ""
		if page != 2 {
			next = fmt.Sprintf("large-cursor-%d", page)
		}
		pages[cursor] = listPage(next, sessions...)
		cursor = next
	}
	configurePages(launcher, pages)
	_, err := inspectRef(t, driver, "missing")
	requireProviderCode(t, err, provider.ErrorProtocolIncompatible)
}

func TestLaterPageRPCErrorsUseClosedClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		rpc  *acp.RPCError
		code provider.ProviderErrorCode
	}{
		"authentication":     {rpc: &acp.RPCError{Code: -32000, Message: "login required"}, code: provider.ErrorAuthenticationRequired},
		"transport protocol": {rpc: &acp.RPCError{Code: -32602, Message: "bad params"}, code: provider.ErrorProtocolIncompatible},
	} {
		t.Run(name, func(t *testing.T) {
			driver, launcher, _ := testDriver(t)
			configurePages(launcher, map[string]scriptedListPage{
				"":      listPage("later", listedSession("target", testOptions("model-a"))),
				"later": {rpcError: tc.rpc},
			})
			_, err := inspectRef(t, driver, "target")
			requireProviderCode(t, err, tc.code)
		})
	}
}

func TestReadinessFullyTraversesAndCachesOnlyValidTerminalTraversal(t *testing.T) {
	driver, launcher, _ := testDriver(t)
	configurePages(launcher, map[string]scriptedListPage{
		"":      listPage("later"),
		"later": listPage(""),
	})
	if got := driver.Readiness(context.Background()); got.State != provider.Ready {
		t.Fatalf("Readiness = %+v", got)
	}
	launcher.mu.Lock()
	child := launcher.children[0]
	methods := append([]string(nil), child.methods...)
	params := append([]json.RawMessage(nil), child.listParams...)
	launcher.mu.Unlock()
	if len(params) != 2 || string(params[0]) != "{}" || string(params[1]) != `{"cursor":"later"}` {
		t.Fatalf("list params = %q", params)
	}
	for _, method := range methods {
		if method != "initialize" && method != "session/list" {
			t.Fatalf("mutating readiness method %q", method)
		}
	}

	badDriver, badLauncher, _ := testDriver(t)
	configurePages(badLauncher, map[string]scriptedListPage{
		"":      listPage("later"),
		"later": {result: map[string]any{"sessions": nil}},
	})
	if got := badDriver.Readiness(context.Background()); got.State != provider.ProtocolIncompatible {
		t.Fatalf("malformed readiness = %+v", got)
	}
	badLauncher.mu.Lock()
	children := len(badLauncher.children)
	badLauncher.mu.Unlock()
	if got := badDriver.Readiness(context.Background()); got.State != provider.ProtocolIncompatible {
		t.Fatalf("second malformed readiness = %+v", got)
	}
	badLauncher.mu.Lock()
	defer badLauncher.mu.Unlock()
	if len(badLauncher.children) != children+1 {
		t.Fatal("invalid traversal was cached")
	}
}
