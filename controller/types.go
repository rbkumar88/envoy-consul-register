package controller

// FactoryAdapter has a method to work with Controller resources.
type ConsulServiceConfig struct {
	ID    string
	Name  string
	Port  int
	IP    string
	Tags  []string
	Attrs map[string]string
	ContainerID string
	ConsulKVStoreKeyName string
	GrpcServiceVerify bool
	EnvoyClusterHeaderName string
	EnvoyClusterConnectTimeOutInMs string
	EnvoyDynamicConfig *EnvoyServiceConfig
	ServiceName string
}
