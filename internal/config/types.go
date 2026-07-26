package config

import "time"

const (
	Version1          = 1
	AccessContentOnly = "content-only"
)

type optional[T any] struct {
	value T
	set   bool
}

func present[T any](value T) optional[T] {
	return optional[T]{value: value, set: true}
}

type Config struct {
	path    string
	exists  bool
	version int
	client  Client
	server  Server
	viewer  Viewer
	agent   Agent
}

func (config Config) Path() string   { return config.path }
func (config Config) Exists() bool   { return config.exists }
func (config Config) Version() int   { return config.version }
func (config Config) Client() Client { return config.client }
func (config Config) Server() Server { return config.server }
func (config Config) Viewer() Viewer { return config.viewer }
func (config Config) Agent() Agent   { return config.agent.clone() }

type Client struct {
	server  optional[string]
	timeout optional[time.Duration]
}

func (client Client) Server() (string, bool)         { return client.server.value, client.server.set }
func (client Client) Timeout() (time.Duration, bool) { return client.timeout.value, client.timeout.set }

type Server struct {
	host                 optional[string]
	port                 optional[int]
	storage              optional[string]
	cleanupInterval      optional[time.Duration]
	defaultExpiresIn     optional[int64]
	shutdownTimeout      optional[time.Duration]
	logMode              optional[string]
	maxWhiteboardBytes   optional[int64]
	maxContextBytes      optional[int64]
	maxImageBytes        optional[int64]
	maxImageRequestBytes optional[int64]
}

func (server Server) Host() (string, bool)    { return server.host.value, server.host.set }
func (server Server) Port() (int, bool)       { return server.port.value, server.port.set }
func (server Server) Storage() (string, bool) { return server.storage.value, server.storage.set }
func (server Server) CleanupInterval() (time.Duration, bool) {
	return server.cleanupInterval.value, server.cleanupInterval.set
}
func (server Server) DefaultExpiresIn() (int64, bool) {
	return server.defaultExpiresIn.value, server.defaultExpiresIn.set
}
func (server Server) ShutdownTimeout() (time.Duration, bool) {
	return server.shutdownTimeout.value, server.shutdownTimeout.set
}
func (server Server) LogMode() (string, bool) { return server.logMode.value, server.logMode.set }
func (server Server) MaxWhiteboardBytes() (int64, bool) {
	return server.maxWhiteboardBytes.value, server.maxWhiteboardBytes.set
}
func (server Server) MaxContextBytes() (int64, bool) {
	return server.maxContextBytes.value, server.maxContextBytes.set
}
func (server Server) MaxImageBytes() (int64, bool) {
	return server.maxImageBytes.value, server.maxImageBytes.set
}
func (server Server) MaxImageRequestBytes() (int64, bool) {
	return server.maxImageRequestBytes.value, server.maxImageRequestBytes.set
}

type Viewer struct {
	localAgent LocalAgent
}

func (viewer Viewer) LocalAgent() LocalAgent { return viewer.localAgent }

type LocalAgent struct {
	enabled optional[bool]
}

func (agent LocalAgent) Enabled() (bool, bool) { return agent.enabled.value, agent.enabled.set }

type Agent struct {
	port                optional[int]
	trustedOrigins      optional[[]string]
	providerIdleTimeout optional[time.Duration]
	shutdownTimeout     optional[time.Duration]
	defaultAccess       optional[string]
}

func (agent Agent) Port() (int, bool) { return agent.port.value, agent.port.set }
func (agent Agent) TrustedOrigins() ([]string, bool) {
	if !agent.trustedOrigins.set {
		return nil, false
	}
	return append([]string(nil), agent.trustedOrigins.value...), true
}
func (agent Agent) ProviderIdleTimeout() (time.Duration, bool) {
	return agent.providerIdleTimeout.value, agent.providerIdleTimeout.set
}
func (agent Agent) ShutdownTimeout() (time.Duration, bool) {
	return agent.shutdownTimeout.value, agent.shutdownTimeout.set
}
func (agent Agent) DefaultAccess() (string, bool) {
	return agent.defaultAccess.value, agent.defaultAccess.set
}
func (agent Agent) clone() Agent {
	if agent.trustedOrigins.set {
		agent.trustedOrigins.value = append([]string(nil), agent.trustedOrigins.value...)
	}
	return agent
}

type BuiltinValues struct {
	Client ClientValues
	Server ServerValues
	Viewer ViewerValues
	Agent  AgentValues
}

type ClientValues struct {
	Server  string
	Timeout time.Duration
}

type ServerValues struct {
	Host                 string
	Port                 int
	Storage              string
	CleanupInterval      time.Duration
	DefaultExpiresIn     int64
	ShutdownTimeout      time.Duration
	LogMode              string
	MaxWhiteboardBytes   int64
	MaxContextBytes      int64
	MaxImageBytes        int64
	MaxImageRequestBytes int64
}

type ViewerValues struct {
	LocalAgent LocalAgentValues
}

type LocalAgentValues struct {
	Enabled bool
}

type AgentValues struct {
	Port                int
	TrustedOrigins      []string
	ProviderIdleTimeout time.Duration
	ShutdownTimeout     time.Duration
	DefaultAccess       string
}
