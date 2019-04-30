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
		for _, elem := range envoyConfig.Clusters {
			if strings.EqualFold(elem.Name, serviceConfig.ContainerID) {
				isClusterFound = true
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

		if env:= getValueFromTag(serviceConfig.Tags,"consul.register/hostname") ; env != "" {
			newClusterWithHostName, err := r.buildEnvoyClusterConfigWithHostName(serviceConfig)
			if err != nil {
				log.Println("Error while creating Envoy Cluster Config with hostname:", err)
				return err
			}
			envoyConfig.Clusters = append(envoyConfig.Clusters, *newClusterWithHostName)
		}

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
									},
								},
							}
						}
						routeSpecifier.RouteConfig.VirtualHosts[0].Routes = append(routeSpecifier.RouteConfig.VirtualHosts[0].Routes, *route)
						log.Printf("Adding new service %s to envoy Route config,%v", serviceConfig.ContainerID,httpConnectionManager)
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
	port, _ := strconv.ParseUint(getopt(serviceConfig.Tags, serviceConfig.IP, "consul.register/nodePort"), 10, 32)
	cluster := &envoyApi.Cluster{
		Name:                 serviceConfig.ContainerID + "-hostname-" + getopt(serviceConfig.Tags, serviceConfig.IP, "node"),
		ConnectTimeout:       time.Duration(connectTimeOutInSeconds) * time.Second,
		ClusterDiscoveryType: &envoyApi.Cluster_Type{Type: envoyApi.Cluster_STRICT_DNS},
		LbPolicy:             envoyApi.Cluster_ROUND_ROBIN,
		LoadAssignment: &envoyApi.ClusterLoadAssignment{
			ClusterName: serviceConfig.ContainerID + "-hostname-" + getopt(serviceConfig.Tags, serviceConfig.IP, "node"),
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
	}
	return cluster, nil
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
					log.Printf("Removing cluster %s from envoy Cluster config, containerID: %s", cluster.Name,serviceConfig.ContainerID)
					envoyConfig.Clusters = append(envoyConfig.Clusters[:i],
						envoyConfig.Clusters[i+1:]...)
				}
			}
			for i := len(envoyConfig.Clusters) - 1; i >= 0; i-- {
				cluster := envoyConfig.Clusters[i]
				if strings.ContainsAny(cluster.Name,"-hostname-") && strings.HasPrefix(serviceConfig.ContainerID, before(cluster.Name,"-hostname-") ) {
					isClusterFound = true
					log.Printf("Removing cluster %s from envoy Cluster config with hostname,containerID: %s", cluster.Name,serviceConfig.ContainerID)
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
	if env:= getValueFromTag(tags,"consul.register/hostname") ; env != "" {
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
