// Package agentlimits owns the fixed resource limits shared by the local
// browser protocol, attachment storage, and native provider adapters.
package agentlimits

const (
	MaxImagesPerTurn          = 8
	MaxImageBytes             = 10 << 20
	MaxTurnImageBytes         = 20 << 20
	MaxConversationImageBytes = int64(512 << 20)
	MaxImageNameBytes         = 255
)
