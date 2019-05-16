package controller

import (
	consulApi "github.com/hashicorp/consul/api"

	envoyApi "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoyCoreApi "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoyEndpointApi "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	envoyListenerApi "github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	envoyRouteApi "github.com/envoyproxy/go-control-plane/envoy/api/v2/route"
	envoyBootstrapApi "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v2"
	httpConnManager "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/http_connection_manager/v2"
	rateLimit "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/rate_limit/v2"
	tcpProxy "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/tcp_proxy/v2"
	fileAccessLog "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v2"
	httpRouter "github.com/envoyproxy/go-control-plane/envoy/config/filter/http/router/v2"

	"io/ioutil"
	"strings"
	"log"
	"github.com/envoyproxy/go-control-plane/pkg/util"
	"github.com/gogo/protobuf/types"
	"fmt"
	"strconv"
	"time"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/envoyproxy/go-control-plane/envoy/config/filter/network/http_connection_manager/v2"
	"github.com/gogo/protobuf/proto"
)

const (
	NodePortSuffix = "-nodePort"
	HostnameLabel  = "consul.register/hostname"
	RoutePathLabel = "consul.register/routePath"
)

type ConsulEnvoyAdapter struct {
	client *consulApi.Client
}

type KV interface {
	Get(key string, q *consulApi.QueryOptions) (*consulApi.KVPair, *consulApi.QueryMeta, error)
	Put(p *consulApi.KVPair, q *consulApi.WriteOptions) (*consulApi.WriteMeta, error)
}

func NewConsulEnvoyAdapter(consulApiClient *consulApi.Client) *ConsulEnvoyAdapter {
	consulEnvoyAdapter := &ConsulEnvoyAdapter{
		client: consulApiClient,
	}
	return consulEnvoyAdapter
}

func (c *ConsulEnvoyAdapter) Get(key string, q *consulApi.QueryOptions) (*consulApi.KVPair, *consulApi.QueryMeta, error) {
	return c.client.KV().Get(key, q)
}

func (c *ConsulEnvoyAdapter) Put(p *consulApi.KVPair, q *consulApi.WriteOptions) (*consulApi.WriteMeta, error) {
	return c.client.KV().Put(p, q)
}
func (r *ConsulEnvoyAdapter) BuildAndStoreEnvoyConfig(serviceConfig *ConsulServiceConfig) error {
	kv, _, _ := r.client.KV().Get(serviceConfig.ConsulKVStoreKeyName, nil)
	serviceConfigError := r.updateEnvoyServiceConfigFromConsulKV(serviceConfig)
	if serviceConfigError != nil {
		log.Println("key found : Error while unmarshaling service Config :", serviceConfigError)
		return serviceConfigError
	}
	envoyConfig := &envoyBootstrapApi.Bootstrap_StaticResources{}
	resolver := funcResolver(func(turl string) (proto.Message, error) {
		switch turl {
		case "type.googleapis.com/envoy.config.filter.network.rate_limit.v2.RateLimit":
			return new(rateLimit.RateLimit), nil
		case "type.googleapis.com/envoy.config.filter.network.tcp_proxy.v2.TcpProxy":
			return new(tcpProxy.TcpProxy), nil
		case "type.googleapis.com/envoy.config.accesslog.v2.FileAccessLog":
			return new(fileAccessLog.FileAccessLog), nil
		case "type.googleapis.com/envoy.config.filter.network.http_connection_manager.v2.HttpConnectionManager":
			return new(httpConnManager.HttpConnectionManager), nil
		case "type.googleapis.com/envoy.config.filter.http.router.v2.Router":
			return new(httpRouter.Router), nil
		}
		return new(httpConnManager.HttpConnectionManager), nil
	})

	u := &jsonpb.Unmarshaler{AnyResolver: resolver}
	if kv != nil {
		if err2 := u.Unmarshal(strings.NewReader(string(kv.Value)), envoyConfig); err2 != nil {
			log.Println("key found : Error while unmarshaling envoy Config :", err2)
			return err2
		}
	} else {
		data, err := ioutil.ReadFile("/etc/envoy_base_config.json")
		_, err1 := r.client.KV().Put(&consulApi.KVPair{Key: serviceConfig.ConsulKVStoreKeyName, Value: []byte(data)}, nil)
		if err1 != nil {
			log.Println("consulkv: failed to store envoy config json:", err)
			return err
		}
		if err2 := jsonpb.Unmarshal(strings.NewReader(string(data)), envoyConfig); err2 != nil {
			log.Println("key not found : Error while unmarshaling envoy Config :", err2)
			return err2
		}
	}
	isClusterFound := false
	isClusterWithHostNameFound := false
	if len(envoyConfig.Clusters) > 0 {
		for _, elem := range envoyConfig.Clusters {
			if strings.EqualFold(elem.Name, serviceConfig.ContainerID) {
				isClusterFound = true
			}
			if env := getValueFromTag(serviceConfig.Tags, HostnameLabel); env != "" {
				if strings.EqualFold(elem.Name, getValueFromTag(serviceConfig.Tags, "container")+NodePortSuffix) {
					isClusterWithHostNameFound = true
				}
			} else {
				isClusterWithHostNameFound = true
			}
		}
	}
	if !isClusterFound {
		log.Printf("Adding new service %s to envoy Cluster config", serviceConfig.ContainerID)
		newCluster, err := r.buildEnvoyClusterConfigWithIP(serviceConfig)
		if err != nil {
			log.Println("Error while creating Envoy Cluster Config:", err)
			return err
		}
		envoyConfig.Clusters = append(envoyConfig.Clusters, *newCluster)

		// Adding new service to envoy Route Config
		for _, listener := range envoyConfig.Listeners {
			for _, filter := range listener.FilterChains[0].Filters {
				if filter.Name != util.HTTPConnectionManager {
					continue
				}
				switch x := filter.ConfigType.(type) {
				case *envoyListenerApi.Filter_Config:
					routeConfig := x.Config.Fields["route_config"]
					virtualHosts := routeConfig.GetStructValue().Fields["virtual_hosts"]
					routes := virtualHosts.GetListValue().Values[0].GetStructValue().Fields["routes"]
					//log.Printf("existing route config : %v",routes)
					newRoute := map[string]*types.Value{
						"match": {Kind: &types.Value_StructValue{StructValue: &types.Struct{
							Fields: map[string]*types.Value{
								"path": {Kind: &types.Value_StringValue{StringValue: serviceConfig.ContainerID}},
								"grpc": {Kind: &types.Value_StructValue{StructValue: &types.Struct{Fields: map[string]*types.Value{}}}},
							},
						}}},
						"route": {Kind: &types.Value_StructValue{StructValue: &types.Struct{
							Fields: map[string]*types.Value{
								"cluster_header": {Kind: &types.Value_StringValue{StringValue: serviceConfig.EnvoyClusterHeaderName}},
							},
						}}},
					}
					if !serviceConfig.GrpcServiceVerify {
						newRoute = map[string]*types.Value{
							"match": {Kind: &types.Value_StructValue{StructValue: &types.Struct{
								Fields: map[string]*types.Value{
									"path": {Kind: &types.Value_StringValue{StringValue: serviceConfig.ContainerID}},
								},
							}}},
							"route": {Kind: &types.Value_StructValue{StructValue: &types.Struct{
								Fields: map[string]*types.Value{
									"cluster_header": {Kind: &types.Value_StringValue{StringValue: serviceConfig.EnvoyClusterHeaderName}},
								},
							}}},
						}
					}
					route := &types.Value{Kind: &types.Value_StructValue{StructValue: &types.Struct{
						Fields: newRoute}}}
					log.Printf("Adding new service %s to envoy Route config", serviceConfig.ContainerID)
					routes.GetListValue().Values = append(routes.GetListValue().Values, route)
				case *envoyListenerApi.Filter_TypedConfig:
					httpConnectionManager := &v2.HttpConnectionManager{}
					// use typed config if available
					err1 := types.UnmarshalAny(x.TypedConfig, httpConnectionManager)
					if err1 != nil || httpConnectionManager == nil {
						log.Println("Error while parsing Envoy HttpConnectionManager TypedConfig:", err)
						return err
					}
					switch routeSpecifier := httpConnectionManager.RouteSpecifier.(type) {
					case *v2.HttpConnectionManager_RouteConfig:
						route := &envoyRouteApi.Route{
							Match: envoyRouteApi.RouteMatch{
								PathSpecifier: &envoyRouteApi.RouteMatch_Path{
									Path: serviceConfig.ContainerID,
								},
								Grpc: &envoyRouteApi.RouteMatch_GrpcRouteMatchOptions{},
							},
							Action: &envoyRouteApi.Route_Route{
								Route: &envoyRouteApi.RouteAction{
									ClusterSpecifier: &envoyRouteApi.RouteAction_ClusterHeader{
										ClusterHeader: serviceConfig.EnvoyClusterHeaderName,
									},
									RetryPolicy: serviceConfig.EnvoyDynamicConfig.RetryPolicy,
								},
							},
						}
						if !serviceConfig.GrpcServiceVerify {
							route = &envoyRouteApi.Route{
								Match: envoyRouteApi.RouteMatch{
									PathSpecifier: &envoyRouteApi.RouteMatch_Path{
										Path: serviceConfig.ContainerID,
									},
								},
								Action: &envoyRouteApi.Route_Route{
									Route: &envoyRouteApi.RouteAction{
										ClusterSpecifier: &envoyRouteApi.RouteAction_ClusterHeader{
											ClusterHeader: serviceConfig.EnvoyClusterHeaderName,
										},
										RetryPolicy: serviceConfig.EnvoyDynamicConfig.RetryPolicy,
									},
								},
							}
						}
						routeSpecifier.RouteConfig.VirtualHosts[0].Routes = append(routeSpecifier.RouteConfig.VirtualHosts[0].Routes, *route)
						log.Printf("Adding new service %s to envoy Route config,%v", serviceConfig.ContainerID, httpConnectionManager)
						x.TypedConfig, _ = types.MarshalAny(httpConnectionManager)
					case *v2.HttpConnectionManager_Rds:
					default:
						return fmt.Errorf("Unsupported HttpConnectionManager RouteConfig %T", x)

					}

				case nil:
				default:
					return fmt.Errorf("Filter.ConfigType has unexpected type %T", x)
				}
			}
		}
		m := &jsonpb.Marshaler{OrigName: true}
		marshaledString, err := m.MarshalToString(envoyConfig)
		_, err1 := r.client.KV().Put(&consulApi.KVPair{Key: serviceConfig.ConsulKVStoreKeyName, Value: []byte(marshaledString)}, nil)
		if err1 != nil {
			log.Println("consulkv: failed to store envoy config json:", err)
			return err1
		}
	}
	if !isClusterWithHostNameFound {
		log.Printf("Adding new service with hostName %s to envoy Cluster config", serviceConfig.ContainerID)

		if env := getValueFromTag(serviceConfig.Tags, HostnameLabel); env != "" {
			newClusterWithHostName, err := r.buildEnvoyClusterConfigWithHostName(serviceConfig)
			if err != nil {
				log.Println("Error while creating Envoy Cluster Config with hostname:", err)
				return err
			}
			envoyConfig.Clusters = append(envoyConfig.Clusters, *newClusterWithHostName)
		}
		containerName := getValueFromTag(serviceConfig.Tags, "container") + NodePortSuffix
		// Adding new service to envoy Route Config
		for _, listener := range envoyConfig.Listeners {
			for _, filter := range listener.FilterChains[0].Filters {
				if filter.Name != util.HTTPConnectionManager {
					continue
				}
				switch x := filter.ConfigType.(type) {
				case *envoyListenerApi.Filter_Config:
					routeConfig := x.Config.Fields["route_config"]
					virtualHosts := routeConfig.GetStructValue().Fields["virtual_hosts"]
					routes := virtualHosts.GetListValue().Values[0].GetStructValue().Fields["routes"]
					//log.Printf("existing route config : %v",routes)
					newRoute := map[string]*types.Value{
						"match": {Kind: &types.Value_StructValue{StructValue: &types.Struct{
							Fields: map[string]*types.Value{
								"path": {Kind: &types.Value_StringValue{StringValue: "/" + getValueFromTag(serviceConfig.Tags, RoutePathLabel)}},
								"grpc": {Kind: &types.Value_StructValue{StructValue: &types.Struct{Fields: map[string]*types.Value{}}}},
							},
						}}},
						"route": {Kind: &types.Value_StructValue{StructValue: &types.Struct{
							Fields: map[string]*types.Value{
								"cluster": {Kind: &types.Value_StringValue{StringValue: containerName}},
							},
						}}},
					}
					if !serviceConfig.GrpcServiceVerify {
						newRoute = map[string]*types.Value{
							"match": {Kind: &types.Value_StructValue{StructValue: &types.Struct{
								Fields: map[string]*types.Value{
									"path": {Kind: &types.Value_StringValue{StringValue: "/" + getValueFromTag(serviceConfig.Tags, RoutePathLabel)}},
								},
							}}},
							"route": {Kind: &types.Value_StructValue{StructValue: &types.Struct{
								Fields: map[string]*types.Value{
									"cluster": {Kind: &types.Value_StringValue{StringValue: containerName}},
								},
							}}},
						}
					}
					route := &types.Value{Kind: &types.Value_StructValue{StructValue: &types.Struct{
						Fields: newRoute}}}
					log.Printf("Adding new service %s to envoy Route config", serviceConfig.ContainerID)
					routes.GetListValue().Values = append(routes.GetListValue().Values, route)
				case *envoyListenerApi.Filter_TypedConfig:
					httpConnectionManager := &v2.HttpConnectionManager{}
					// use typed config if available
					err1 := types.UnmarshalAny(x.TypedConfig, httpConnectionManager)
					if err1 != nil || httpConnectionManager == nil {
						log.Println("Error while parsing Envoy HttpConnectionManager TypedConfig:", err1)
						return err1
					}
					switch routeSpecifier := httpConnectionManager.RouteSpecifier.(type) {
					case *v2.HttpConnectionManager_RouteConfig:
						route := &envoyRouteApi.Route{
							Match: envoyRouteApi.RouteMatch{
								PathSpecifier: &envoyRouteApi.RouteMatch_Path{
									Path: "/" + getValueFromTag(serviceConfig.Tags, RoutePathLabel),
								},
								Grpc: &envoyRouteApi.RouteMatch_GrpcRouteMatchOptions{},
							},
							Action: &envoyRouteApi.Route_Route{
								Route: &envoyRouteApi.RouteAction{
									ClusterSpecifier: &envoyRouteApi.RouteAction_Cluster{
										Cluster: containerName,
									},
									RetryPolicy: serviceConfig.EnvoyDynamicConfig.RetryPolicy,
								},
							},
						}
						if !serviceConfig.GrpcServiceVerify {
							route = &envoyRouteApi.Route{
								Match: envoyRouteApi.RouteMatch{
									PathSpecifier: &envoyRouteApi.RouteMatch_Path{
										Path: "/" + getValueFromTag(serviceConfig.Tags, RoutePathLabel),
									},
								},
								Action: &envoyRouteApi.Route_Route{
									Route: &envoyRouteApi.RouteAction{
										ClusterSpecifier: &envoyRouteApi.RouteAction_Cluster{
											Cluster: containerName,
										},
										RetryPolicy: serviceConfig.EnvoyDynamicConfig.RetryPolicy,
									},
								},
							}
						}
						routeSpecifier.RouteConfig.VirtualHosts[0].Routes = append(routeSpecifier.RouteConfig.VirtualHosts[0].Routes, *route)
						log.Printf("Adding new service %s to envoy Route config,%v", serviceConfig.ContainerID, httpConnectionManager)
						x.TypedConfig, _ = types.MarshalAny(httpConnectionManager)
					case *v2.HttpConnectionManager_Rds:
					default:
						return fmt.Errorf("Unsupported HttpConnectionManager RouteConfig %T ", x)

					}

				case nil:
				default:
					return fmt.Errorf("Filter.ConfigType has unexpected type %T", x)
				}
			}
		}
		m := &jsonpb.Marshaler{OrigName: true}
		marshaledString, err := m.MarshalToString(envoyConfig)
		_, err1 := r.client.KV().Put(&consulApi.KVPair{Key: serviceConfig.ConsulKVStoreKeyName, Value: []byte(marshaledString)}, nil)
		if err1 != nil {
			log.Println("consulkv: failed to store envoy config json:", err)
			return err1
		}
	}
	return nil
}
func (r *ConsulEnvoyAdapter) buildEnvoyClusterConfigWithIP(serviceConfig *ConsulServiceConfig) (*envoyApi.Cluster, error) {
	http2ProtocolOptions := new(envoyCoreApi.Http2ProtocolOptions)
	if !serviceConfig.GrpcServiceVerify {
		http2ProtocolOptions = nil
	}
	connectTimeOutInSeconds, err := strconv.Atoi(serviceConfig.EnvoyClusterConnectTimeOutInMs)
	if err != nil {
		log.Println("Error while parsing Envoy Cluster Connect Timeout:", err)
		return nil, err
	}
	cluster := &envoyApi.Cluster{
		Name:                 serviceConfig.ContainerID,
		ConnectTimeout:       time.Duration(connectTimeOutInSeconds) * time.Second,
		ClusterDiscoveryType: &envoyApi.Cluster_Type{Type: envoyApi.Cluster_STRICT_DNS},
		LbPolicy:             envoyApi.Cluster_ROUND_ROBIN,
		LoadAssignment: &envoyApi.ClusterLoadAssignment{
			ClusterName: serviceConfig.ContainerID,
			Endpoints: []envoyEndpointApi.LocalityLbEndpoints{{
				LbEndpoints: []envoyEndpointApi.LbEndpoint{{
					HostIdentifier: &envoyEndpointApi.LbEndpoint_Endpoint{
						Endpoint: &envoyEndpointApi.Endpoint{
							Address: &envoyCoreApi.Address{
								Address: &envoyCoreApi.Address_SocketAddress{
									SocketAddress: &envoyCoreApi.SocketAddress{
										Address: serviceConfig.IP,
										PortSpecifier: &envoyCoreApi.SocketAddress_PortValue{
											PortValue: uint32(serviceConfig.Port),
										},
									},
								},
							},
						},
					},
				}},
			}},
		},
		Http2ProtocolOptions: http2ProtocolOptions,
		HealthChecks:         serviceConfig.EnvoyDynamicConfig.HealthChecks,
		TlsContext:           serviceConfig.EnvoyDynamicConfig.TlsContext,
	}
	return cluster, nil
}
func (r *ConsulEnvoyAdapter) buildEnvoyClusterConfigWithHostName(serviceConfig *ConsulServiceConfig) (*envoyApi.Cluster, error) {
	http2ProtocolOptions := new(envoyCoreApi.Http2ProtocolOptions)
	if !serviceConfig.GrpcServiceVerify {
		http2ProtocolOptions = nil
	}
	connectTimeOutInSeconds, err := strconv.Atoi(serviceConfig.EnvoyClusterConnectTimeOutInMs)
	if err != nil {
		log.Println("Error while parsing Envoy Cluster Connect Timeout:", err)
		return nil, err
	}
	containerName := getValueFromTag(serviceConfig.Tags, "container") + NodePortSuffix
	port, _ := strconv.ParseUint(getopt(serviceConfig.Tags, serviceConfig.IP, "consul.register/nodePort"), 10, 32)
	cluster := &envoyApi.Cluster{
		Name:                 containerName,
		ConnectTimeout:       time.Duration(connectTimeOutInSeconds) * time.Second,
		ClusterDiscoveryType: &envoyApi.Cluster_Type{Type: envoyApi.Cluster_STRICT_DNS},
		LbPolicy:             envoyApi.Cluster_ROUND_ROBIN,
		LoadAssignment: &envoyApi.ClusterLoadAssignment{
			ClusterName: containerName,
			Endpoints: []envoyEndpointApi.LocalityLbEndpoints{{
				LbEndpoints: []envoyEndpointApi.LbEndpoint{{
					HostIdentifier: &envoyEndpointApi.LbEndpoint_Endpoint{
						Endpoint: &envoyEndpointApi.Endpoint{
							Address: &envoyCoreApi.Address{
								Address: &envoyCoreApi.Address_SocketAddress{
									SocketAddress: &envoyCoreApi.SocketAddress{
										Address: getopt(serviceConfig.Tags, serviceConfig.IP, "node"),
										PortSpecifier: &envoyCoreApi.SocketAddress_PortValue{
											PortValue: uint32(port),
										},
									},
								},
							},
						},
					},
				}},
			}},
		},
		Http2ProtocolOptions: http2ProtocolOptions,
		HealthChecks:         serviceConfig.EnvoyDynamicConfig.HealthChecks,
		TlsContext:           serviceConfig.EnvoyDynamicConfig.TlsContext,
	}
	return cluster, nil
}

func (r *ConsulEnvoyAdapter) BuildAndUpdateEnvoyConfig(serviceConfig *ConsulServiceConfig) error {
	kv, _, _ := r.client.KV().Get(serviceConfig.ConsulKVStoreKeyName, nil)
	m := &jsonpb.Marshaler{OrigName: true}
	marshaledString, _ := m.MarshalToString(serviceConfig.EnvoyDynamicConfig)
	_, err1 := r.client.KV().Put(&consulApi.KVPair{Key: serviceConfig.ServiceName, Value: []byte(marshaledString)}, nil)
	if err1 != nil {
		log.Printf("consulkv: failed to store service config for %s: %s \n", serviceConfig.ServiceName, err1)
		return err1
	}
	envoyConfig := &envoyBootstrapApi.Bootstrap_StaticResources{}
	resolver := funcResolver(func(turl string) (proto.Message, error) {
		switch turl {
		case "type.googleapis.com/envoy.config.filter.network.rate_limit.v2.RateLimit":
			return new(rateLimit.RateLimit), nil
		case "type.googleapis.com/envoy.config.filter.network.tcp_proxy.v2.TcpProxy":
			return new(tcpProxy.TcpProxy), nil
		case "type.googleapis.com/envoy.config.accesslog.v2.FileAccessLog":
			return new(fileAccessLog.FileAccessLog), nil
		case "type.googleapis.com/envoy.config.filter.network.http_connection_manager.v2.HttpConnectionManager":
			return new(httpConnManager.HttpConnectionManager), nil
		case "type.googleapis.com/envoy.config.filter.http.router.v2.Router":
			return new(httpRouter.Router), nil
		}
		return new(httpConnManager.HttpConnectionManager), nil
	})

	u := &jsonpb.Unmarshaler{AnyResolver: resolver}
	if kv != nil {
		if err2 := u.Unmarshal(strings.NewReader(string(kv.Value)), envoyConfig); err2 != nil {
			log.Println("key found : Error while unmarshaling envoy Config :", err2)
			return err2
		}
	} else {
		data, err := ioutil.ReadFile("/etc/envoy_base_config.json")
		_, err1 := r.client.KV().Put(&consulApi.KVPair{Key: serviceConfig.ConsulKVStoreKeyName, Value: []byte(data)}, nil)
		if err1 != nil {
			log.Println("consulkv: failed to store envoy config json:", err)
			return err
		}
		if err2 := jsonpb.Unmarshal(strings.NewReader(string(data)), envoyConfig); err2 != nil {
			log.Println("key not found : Error while unmarshaling envoy Config :", err2)
			return err2
		}
	}
	isClusterFound := false
	if len(envoyConfig.Clusters) > 0 {
		for _, cluster := range envoyConfig.Clusters {
			if strings.EqualFold(r.getServiceNameFromConsul(cluster.Name, serviceConfig), serviceConfig.ServiceName) ||
				strings.EqualFold(before(cluster.Name, NodePortSuffix), serviceConfig.ServiceName) {
				isClusterFound = true
				updateEnvoyClusterConfig(serviceConfig, cluster)
				log.Printf("Update envoy Cluster config for %s", serviceConfig.ServiceName)
			}
		}
	}
	if isClusterFound {
		// Update envoy Route Config
		for _, listener := range envoyConfig.Listeners {
			for _, filter := range listener.FilterChains[0].Filters {
				if filter.Name != util.HTTPConnectionManager {
					continue
				}
				switch x := filter.ConfigType.(type) {
				case *envoyListenerApi.Filter_Config:
					routeConfig := x.Config.Fields["route_config"]
					virtualHosts := routeConfig.GetStructValue().Fields["virtual_hosts"]
					routes := virtualHosts.GetListValue().Values[0].GetStructValue().Fields["routes"]
					//log.Printf("existing route config : %v",routes)
					newRoute := map[string]*types.Value{
						"match": {Kind: &types.Value_StructValue{StructValue: &types.Struct{
							Fields: map[string]*types.Value{
								"path": {Kind: &types.Value_StringValue{StringValue: serviceConfig.ContainerID}},
								"grpc": {Kind: &types.Value_StructValue{StructValue: &types.Struct{Fields: map[string]*types.Value{}}}},
							},
						}}},
						"route": {Kind: &types.Value_StructValue{StructValue: &types.Struct{
							Fields: map[string]*types.Value{
								"cluster_header": {Kind: &types.Value_StringValue{StringValue: serviceConfig.EnvoyClusterHeaderName}},
							},
						}}},
					}
					if !serviceConfig.GrpcServiceVerify {
						newRoute = map[string]*types.Value{
							"match": {Kind: &types.Value_StructValue{StructValue: &types.Struct{
								Fields: map[string]*types.Value{
									"path": {Kind: &types.Value_StringValue{StringValue: serviceConfig.ContainerID}},
								},
							}}},
							"route": {Kind: &types.Value_StructValue{StructValue: &types.Struct{
								Fields: map[string]*types.Value{
									"cluster_header": {Kind: &types.Value_StringValue{StringValue: serviceConfig.EnvoyClusterHeaderName}},
								},
							}}},
						}
					}
					route := &types.Value{Kind: &types.Value_StructValue{StructValue: &types.Struct{
						Fields: newRoute}}}
					log.Printf("Adding new service %s to envoy Route config", serviceConfig.ContainerID)
					routes.GetListValue().Values = append(routes.GetListValue().Values, route)
				case *envoyListenerApi.Filter_TypedConfig:
					httpConnectionManager := &v2.HttpConnectionManager{}
					// use typed config if available
					err1 := types.UnmarshalAny(x.TypedConfig, httpConnectionManager)
					if err1 != nil || httpConnectionManager == nil {
						log.Println("Error while parsing Envoy HttpConnectionManager TypedConfig:", err1)
						return err1
					}
					switch routeSpecifier := httpConnectionManager.RouteSpecifier.(type) {
					case *v2.HttpConnectionManager_RouteConfig:
						if len(routeSpecifier.RouteConfig.VirtualHosts[0].Routes) > 0 {
							for _, route := range routeSpecifier.RouteConfig.VirtualHosts[0].Routes {
								switch pathSpecifier := route.Match.PathSpecifier.(type) {
								case *envoyRouteApi.RouteMatch_Prefix, *envoyRouteApi.RouteMatch_Regex:
									break
								case *envoyRouteApi.RouteMatch_Path:
									if strings.EqualFold(r.getServiceNameFromConsul(pathSpecifier.Path, serviceConfig), serviceConfig.ServiceName) {
										log.Printf("Updating envoy Route config for service %s", serviceConfig.ServiceName)
										updateEnvoyRouteConfig(serviceConfig, route)
									}
								}
							}
							for _, route := range routeSpecifier.RouteConfig.VirtualHosts[0].Routes {
								switch routeAction := route.Action.(type) {
								case *envoyRouteApi.Route_DirectResponse, *envoyRouteApi.Route_Redirect:
									break
								case *envoyRouteApi.Route_Route:
									switch clusterSpecifier := routeAction.Route.ClusterSpecifier.(type) {
									case *envoyRouteApi.RouteAction_ClusterHeader, *envoyRouteApi.RouteAction_WeightedClusters:
										break
									case *envoyRouteApi.RouteAction_Cluster:
										if strings.EqualFold(before(clusterSpecifier.Cluster, NodePortSuffix), serviceConfig.ServiceName) {
											log.Printf("Updating envoy Route config for service with hostname %s", serviceConfig.ServiceName)
											updateEnvoyRouteConfig(serviceConfig, route)
										}
									}
								}
							}
							log.Printf("httpConnManager:%+v \n ",httpConnectionManager)
							x.TypedConfig, _ = types.MarshalAny(httpConnectionManager)
						}
					case *v2.HttpConnectionManager_Rds:
					default:
						return fmt.Errorf("Unsupported HttpConnectionManager RouteConfig %T ", x)

					}
				case nil:
				default:
					return fmt.Errorf("Filter.ConfigType has unexpected type %T", x)
				}
			}
		}
		marshaledString, _ := m.MarshalToString(envoyConfig)
		_, err1 := r.client.KV().Put(&consulApi.KVPair{Key: serviceConfig.ConsulKVStoreKeyName, Value: []byte(marshaledString)}, nil)
		if err1 != nil {
			log.Println("consulkv: failed to store envoy config json:", err1)
			return err1
		}
	}
	return nil
}
func updateEnvoyClusterConfig(serviceConfig *ConsulServiceConfig, cluster envoyApi.Cluster) {
	//http2ProtocolOptions := new(envoyCoreApi.Http2ProtocolOptions)
	//cluster.Http2ProtocolOptions= http2ProtocolOptions
	cluster.HealthChecks = serviceConfig.EnvoyDynamicConfig.HealthChecks
	cluster.TlsContext = serviceConfig.EnvoyDynamicConfig.TlsContext
}

func updateEnvoyRouteConfig(serviceConfig *ConsulServiceConfig, route envoyRouteApi.Route) {
	switch routeAction := route.Action.(type) {
	case *envoyRouteApi.Route_Route:
		routeAction.Route.RetryPolicy = serviceConfig.EnvoyDynamicConfig.RetryPolicy
	}
}

type funcResolver func(turl string) (proto.Message, error)

func (fn funcResolver) Resolve(turl string) (proto.Message, error) {
	return fn(turl)
}
func (r *ConsulEnvoyAdapter) RemoveAndUpdateEnvoyConfig(serviceConfig *ConsulServiceConfig) error {
	kv, _, _ := r.client.KV().Get(serviceConfig.ConsulKVStoreKeyName, nil)
	if kv != nil {
		envoyConfig := &envoyBootstrapApi.Bootstrap_StaticResources{}
		resolver := funcResolver(func(turl string) (proto.Message, error) {
			switch turl {
			case "type.googleapis.com/envoy.config.filter.network.rate_limit.v2.RateLimit":
				return new(rateLimit.RateLimit), nil
			case "type.googleapis.com/envoy.config.filter.network.tcp_proxy.v2.TcpProxy":
				return new(tcpProxy.TcpProxy), nil
			case "type.googleapis.com/envoy.config.accesslog.v2.FileAccessLog":
				return new(fileAccessLog.FileAccessLog), nil
			case "type.googleapis.com/envoy.config.filter.network.http_connection_manager.v2.HttpConnectionManager":
				return new(httpConnManager.HttpConnectionManager), nil
			case "type.googleapis.com/envoy.config.filter.http.router.v2.Router":
				return new(httpRouter.Router), nil
			}
			return new(httpConnManager.HttpConnectionManager), nil
		})

		u := &jsonpb.Unmarshaler{AnyResolver: resolver}
		if err2 := u.Unmarshal(strings.NewReader(string(kv.Value)), envoyConfig); err2 != nil {
			log.Println("key found : Error while unmarshaling envoy Config :", err2)
		}
		isClusterFound := false
		if len(envoyConfig.Clusters) > 0 {
			for i := len(envoyConfig.Clusters) - 1; i >= 0; i-- {
				cluster := envoyConfig.Clusters[i]
				if strings.HasPrefix(serviceConfig.ContainerID, cluster.Name) {
					isClusterFound = true
					log.Printf("Removing service %s from envoy Cluster config", serviceConfig.ContainerID)
					envoyConfig.Clusters = append(envoyConfig.Clusters[:i],
						envoyConfig.Clusters[i+1:]...)
				}
			}
			//Removing service from envoy Route config
			if isClusterFound {
				for _, listener := range envoyConfig.Listeners {
					for _, filter := range listener.FilterChains[0].Filters {
						if filter.Name != util.HTTPConnectionManager {
							continue
						}
						switch x := filter.ConfigType.(type) {
						case *envoyListenerApi.Filter_Config:
							routeConfig := x.Config.Fields["route_config"]
							virtualHosts := routeConfig.GetStructValue().Fields["virtual_hosts"]
							routes := virtualHosts.GetListValue().Values[0].GetStructValue().Fields["routes"]
							for i := len(routes.GetListValue().Values) - 1; i >= 0; i-- {
								route := routes.GetListValue().Values[i]
								routePath := route.GetStructValue().Fields["match"].GetStructValue().Fields["path"]
								if routePath != nil && routePath.GetStringValue() != "" && strings.HasPrefix(serviceConfig.ContainerID, routePath.GetStringValue()) {
									log.Printf("Removing service %s from envoy Route config", serviceConfig.ID)
									routes.GetListValue().Values = append(routes.GetListValue().Values[:i],
										routes.GetListValue().Values[i+1:]...)
								}
							}
						case *envoyListenerApi.Filter_TypedConfig:
							httpConnectionManager := &v2.HttpConnectionManager{}
							// use typed config if available
							err1 := types.UnmarshalAny(x.TypedConfig, httpConnectionManager)
							if err1 != nil || httpConnectionManager == nil {
								log.Println("Error while parsing Envoy HttpConnectionManager TypedConfig:", err1)
								return err1
							}
							switch routeSpecifier := httpConnectionManager.RouteSpecifier.(type) {
							case *v2.HttpConnectionManager_RouteConfig:
								routes := routeSpecifier.RouteConfig.VirtualHosts[0].Routes
								for i := len(routes) - 1; i >= 0; i-- {
									route := routes[i]
									routePath := route.Match.PathSpecifier
									if routePath != nil {
										switch x := routePath.(type) {
										case *envoyRouteApi.RouteMatch_Path:
											if strings.HasPrefix(serviceConfig.ContainerID, x.Path) {
												log.Printf("Removing service %s from envoy Route config", serviceConfig.ID)
												routeSpecifier.RouteConfig.VirtualHosts[0].Routes = append(routeSpecifier.RouteConfig.VirtualHosts[0].Routes[:i], routeSpecifier.RouteConfig.VirtualHosts[0].Routes[i+1:]...)
											}
										}
									}
								}
								x.TypedConfig, _ = types.MarshalAny(httpConnectionManager)
							case *v2.HttpConnectionManager_Rds:
							default:
								return fmt.Errorf("Unsupported HttpConnectionManager RouteConfig %T", x)

							}
						case nil:
						default:
							log.Printf("Filter.ConfigType has unsupported type %T", x)
						}
					}
				}
				m := &jsonpb.Marshaler{OrigName: true}
				marshaledString, err := m.MarshalToString(envoyConfig)
				_, err1 := r.client.KV().Put(&consulApi.KVPair{Key: serviceConfig.ConsulKVStoreKeyName, Value: []byte(marshaledString)}, nil)
				if err1 != nil {
					log.Fatalf("consulkv: failed to store envoy config json:%v", err)
				}
			}
		}
	}
	return nil
}
func getValueFromTag(tags []string, searchKey string) string {
	for _, tag := range tags {
		key := strings.Split(tag, ":")
		if len(key) <= 1 {
			continue
		}
		if key[0] == searchKey {
			return key[1]
		}
	}
	return ""
}
func getopt(tags [] string, def string, searchKey string) string {
	if env := getValueFromTag(tags, HostnameLabel); env != "" {
		return getValueFromTag(tags, searchKey)
	}
	return def
}
func before(value string, a string) string {
	// Get substring before a string.
	pos := strings.Index(value, a)
	if pos == -1 {
		return ""
	}
	return value[0:pos]
}
func (r *ConsulEnvoyAdapter) getServiceNameFromConsul(clusterName string, serviceConfig *ConsulServiceConfig) string {
	agentService, _, err := r.client.Agent().Service(clusterName+"-"+serviceConfig.ServiceName, nil)
	if err != nil {
		return serviceConfig.Name
	}
	if agentService != nil && agentService.Tags != nil && len(agentService.Tags) > 0 {
		return getValueFromTag(agentService.Tags, "container")
	} else {
		return serviceConfig.Name
	}
}
func (r *ConsulEnvoyAdapter) updateEnvoyServiceConfigFromConsulKV(serviceConfig *ConsulServiceConfig) error {
	consulServiceName := getValueFromTag(serviceConfig.Tags, "container")
	if consulServiceName == "" {
		consulServiceName = serviceConfig.Name
	}
	if val := consulServiceName; val != "" {
		kv, _, _ := r.client.KV().Get(val, nil)
		if kv != nil {
			envoyDynamicConfig := &EnvoyServiceConfig{}
			u := &jsonpb.Unmarshaler{}
			err := u.Unmarshal(strings.NewReader(string(kv.Value)), envoyDynamicConfig)
			if err != nil {
				return fmt.Errorf("Error while parsing service config: %q ", err)
			}
			serviceConfig.EnvoyDynamicConfig = envoyDynamicConfig
		}
	}
	return nil
}
