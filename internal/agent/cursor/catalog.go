package cursor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
)

const (
	modelProbeTimeout   = 5 * time.Second
	modelProbeStopGrace = 100 * time.Millisecond
	modelProbeKillGrace = 500 * time.Millisecond
	maxModelOutputBytes = provider.MaxCatalogBytes
)

type boundedReadResult struct {
	data []byte
	err  error
}

func readBounded(reader io.Reader, limit int) boundedReadResult {
	data, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil || len(data) > limit {
		return boundedReadResult{err: errors.New("invalid Cursor model output")}
	}
	return boundedReadResult{data: data}
}

func stopModelProbe(child provider.ManagedChild, waited <-chan error, waitDone bool) {
	_ = child.Terminate()
	if !waitDone {
		timer := time.NewTimer(modelProbeStopGrace)
		select {
		case <-waited:
			waitDone = true
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	_ = child.Kill()
	if !waitDone {
		timer := time.NewTimer(modelProbeKillGrace)
		select {
		case <-waited:
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

func (d *Driver) probeCatalog(ctx context.Context) (provider.ModelCatalog, error) {
	timeout := d.probeTimeout
	if timeout <= 0 {
		timeout = modelProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request := common.ProcessRequest{Executable: d.config.Executable, Arguments: []string{"--list-models"}, Environment: append([]string(nil), d.config.Environment...), WorkingDirectory: d.config.ProviderRoot}
	if err := validateCursorExecutable(request.Executable); err != nil {
		return provider.ModelCatalog{}, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	child, err := d.config.Launcher.Launch(probeCtx, request)
	if err != nil || common.IsNil(child) {
		return provider.ModelCatalog{}, provider.NewProviderError(provider.ErrorStartupFailed)
	}
	_ = child.Input().Close()
	stdout := make(chan boundedReadResult, 1)
	stderr := make(chan boundedReadResult, 1)
	waited := make(chan error, 1)
	go func() { stdout <- readBounded(child.Output(), maxModelOutputBytes) }()
	go func() { stderr <- readBounded(child.Errors(), maxModelOutputBytes) }()
	go func() { waited <- child.Wait() }()
	var waitErr error
	var out, errOut boundedReadResult
	waitDone, outDone, errDone := false, false, false
	for !waitDone || !outDone || !errDone {
		select {
		case waitErr = <-waited:
			waitDone = true
			waited = nil
		case out = <-stdout:
			outDone = true
			stdout = nil
		case errOut = <-stderr:
			errDone = true
			stderr = nil
		case <-probeCtx.Done():
			// Stream EOF is part of the process contract. Escalation and reap use
			// an independent bounded window even if the caller's deadline expired.
			stopModelProbe(child, waited, waitDone)
			return provider.ModelCatalog{}, provider.NewProviderError(provider.ErrorStartupFailed)
		}
	}
	if waitErr != nil || out.err != nil || errOut.err != nil {
		return provider.ModelCatalog{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	catalog, err := parseModelCatalog(out.data, true)
	if err != nil {
		return provider.ModelCatalog{}, provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	return catalog, nil
}

func parseModelCatalog(output []byte, images bool) (provider.ModelCatalog, error) {
	if len(output) == 0 || len(output) > maxModelOutputBytes || !utf8.Valid(output) || bytes.IndexByte(output, 0) >= 0 {
		return provider.ModelCatalog{}, errors.New("invalid Cursor model catalog")
	}
	for index, b := range output {
		if b == '\r' && (index+1 >= len(output) || output[index+1] != '\n') {
			return provider.ModelCatalog{}, errors.New("invalid Cursor model catalog")
		}
	}
	for _, r := range string(output) {
		if unicode.IsControl(r) && r != '\n' && r != '\r' {
			return provider.ModelCatalog{}, errors.New("invalid Cursor model catalog")
		}
	}
	text := strings.TrimSuffix(string(output), "\n")
	if strings.HasSuffix(text, "\r") {
		text = strings.TrimSuffix(text, "\r")
	}
	lines := strings.Split(text, "\n")
	catalog := provider.ModelCatalog{Models: make([]provider.CatalogModel, 0, min(len(lines), provider.MaxCatalogModels))}
	seen := make(map[string]struct{}, min(len(lines), provider.MaxCatalogModels))
	current, fallback := -1, -1
	sawHeader, sawTip := false, false
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		if line == "" {
			continue
		}
		if line == "Available models" && len(catalog.Models) == 0 && !sawHeader && !sawTip {
			sawHeader = true
			continue
		}
		if strings.HasPrefix(line, "Tip: ") && len(catalog.Models) > 0 && !sawTip {
			sawTip = true
			continue
		}
		if sawTip || len(catalog.Models) >= provider.MaxCatalogModels {
			return provider.ModelCatalog{}, errors.New("invalid Cursor model catalog")
		}
		slug, name, ok := strings.Cut(line, " - ")
		if !ok || slug == "" || name == "" || len(slug) > provider.MaxModelValueBytes || len(name) > provider.MaxTitleBytes || strings.TrimSpace(slug) != slug || strings.TrimSpace(name) != name {
			return provider.ModelCatalog{}, errors.New("invalid Cursor model row")
		}
		marker := ""
		if strings.HasSuffix(name, " (current)") {
			name = strings.TrimSuffix(name, " (current)")
			marker = "current"
		} else if strings.HasSuffix(name, " (default)") {
			name = strings.TrimSuffix(name, " (default)")
			marker = "default"
		}
		if name == "" {
			return provider.ModelCatalog{}, errors.New("invalid Cursor model row")
		}
		if _, duplicate := seen[slug]; duplicate {
			return provider.ModelCatalog{}, errors.New("duplicate Cursor model")
		}
		seen[slug] = struct{}{}
		index := len(catalog.Models)
		if marker == "current" {
			if current >= 0 {
				return provider.ModelCatalog{}, errors.New("multiple current Cursor models")
			}
			current = index
		} else if marker == "default" {
			if fallback >= 0 {
				return provider.ModelCatalog{}, errors.New("multiple default Cursor models")
			}
			fallback = index
		}
		catalog.Models = append(catalog.Models, provider.CatalogModel{Model: slug, DisplayName: name, DefaultEffort: "default", SupportedReasoningEfforts: []provider.ReasoningEffort{{Value: "default"}}, SupportsImages: images})
	}
	if len(catalog.Models) == 0 {
		return provider.ModelCatalog{}, errors.New("invalid Cursor model catalog")
	}
	selected := current
	if selected < 0 {
		selected = fallback
	}
	if selected < 0 {
		for i := range catalog.Models {
			if catalog.Models[i].Model == "auto" {
				selected = i
				break
			}
		}
	}
	if selected < 0 {
		selected = 0
	}
	catalog.Models[selected].Default = true
	if catalog.Validate() != nil {
		return provider.ModelCatalog{}, errors.New("invalid Cursor model catalog")
	}
	return catalog, nil
}

func defaultSettings(catalog provider.ModelCatalog) (provider.ExecutionSettings, provider.ModelPresentation, error) {
	for _, model := range catalog.Models {
		if model.Default {
			settings := provider.ExecutionSettings{Model: model.Model, Effort: "default", Speed: provider.SpeedStandard}
			return settings, provider.ModelPresentation{ModelDisplayName: model.DisplayName, Selectable: true}, nil
		}
	}
	return provider.ExecutionSettings{}, provider.ModelPresentation{}, errors.New("missing default Cursor model")
}
