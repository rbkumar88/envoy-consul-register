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

//type EnvoyServiceConfig struct {
//	RetryPolicy *route.RetryPolicy `protobuf:"bytes,16,opt,name=retry_policy,json=retryPolicy,proto3" json:"retry_policy,omitempty"`
//	HealthChecks []*core.HealthCheck `protobuf:"bytes,8,rep,name=health_checks,json=healthChecks,proto3" json:"health_checks,omitempty"`
//	TlsContext *auth.UpstreamTlsContext `protobuf:"bytes,11,opt,name=tls_context,json=tlsContext,proto3" json:"tls_context,omitempty"`
//}
