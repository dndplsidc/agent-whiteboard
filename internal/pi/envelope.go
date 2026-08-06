package pi

import (
	"github.com/edocsss/agent-whiteboard/internal/contentturn"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

type Envelope = contentturn.Envelope

const (
	envelopeHeader           = contentturn.Header
	envelopeFooter           = contentturn.Footer
	initialInstructions      = contentturn.ContentOnlyInitialInstructions
	replacementInstructions  = contentturn.ContentOnlyReplacementInstructions
	continuationInstructions = contentturn.ContentOnlyContinuationInstructions
)

var envelopeLabels = func() [14]string {
	var labels [14]string
	copy(labels[:], contentturn.Labels())
	return labels
}()

func BuildEnvelope(request provider.TurnRequest) ([]byte, error) {
	return contentturn.Build(request, contentturn.PolicyContentOnly)
}

func ParseEnvelope(encoded []byte) (Envelope, error) { return contentturn.Parse(encoded) }

func wipe(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
