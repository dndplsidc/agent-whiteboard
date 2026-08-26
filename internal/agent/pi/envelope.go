package pi

import (
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

type Envelope = provider.Envelope

const (
	envelopeHeader           = provider.Header
	envelopeFooter           = provider.Footer
	initialInstructions      = provider.ConfiguredInitialInstructions
	replacementInstructions  = provider.ConfiguredReplacementInstructions
	continuationInstructions = provider.ConfiguredContinuationInstructions
)

var envelopeLabels = func() [14]string {
	var labels [14]string
	copy(labels[:], provider.Labels())
	return labels
}()

func BuildEnvelope(request provider.TurnRequest) ([]byte, error) {
	return provider.Build(request, provider.PolicyConfigured)
}

func ParseEnvelope(encoded []byte) (Envelope, error) { return provider.Parse(encoded) }

func wipe(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
