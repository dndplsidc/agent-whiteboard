package pi

import (
	"github.com/edocsss/agent-whiteboard/internal/contentturn"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

type Envelope = contentturn.Envelope

const (
	envelopeHeader           = contentturn.Header
	envelopeFooter           = contentturn.Footer
	initialInstructions      = contentturn.ConfiguredInitialInstructions
	replacementInstructions  = contentturn.ConfiguredReplacementInstructions
	continuationInstructions = contentturn.ConfiguredContinuationInstructions
)

var envelopeLabels = func() [14]string {
	var labels [14]string
	copy(labels[:], contentturn.Labels())
	return labels
}()

func BuildEnvelope(request provider.TurnRequest) ([]byte, error) {
	return contentturn.Build(request, contentturn.PolicyConfigured)
}

func ParseEnvelope(encoded []byte) (Envelope, error) { return contentturn.Parse(encoded) }

func wipe(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
